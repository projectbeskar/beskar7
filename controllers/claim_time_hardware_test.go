/*
Copyright 2024 The Beskar7 Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

var _ = Describe("Claim-time hardware matching (hardwareRequirements.matchAt: Claim)", func() {
	// 8 cores, 32 GB, 500 GB: meets claimReqs.
	goodReport := func() *infrav1.InspectionReport {
		return &infrav1.InspectionReport{
			CPUs:   []infrav1.CPUInfo{{ID: "0", Cores: 8}},
			Memory: []infrav1.MemoryInfo{{ID: "DIMM0", Capacity: "32GB"}},
			Disks:  []infrav1.DiskInfo{{Name: "sda", SizeGB: 500}},
		}
	}
	// 2 cores: misses the CPU minimum.
	shortReport := func() *infrav1.InspectionReport {
		r := goodReport()
		r.CPUs = []infrav1.CPUInfo{{ID: "0", Cores: 2}}
		return r
	}
	claimReqs := func(at infrav1.HardwareMatchPoint) *infrav1.HardwareRequirements {
		return &infrav1.HardwareRequirements{MatchAt: at, MinCPUCores: 4, MinMemoryGB: 16, MinDiskGB: 100}
	}

	host := func(name, zone string, report *infrav1.InspectionReport) *infrav1.PhysicalHost {
		h := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{Address: "https://192.0.2.10", CredentialsSecretRef: "irrelevant"},
			},
			Status: infrav1.PhysicalHostStatus{State: infrav1.StateAvailable, InspectionReport: report},
		}
		if zone != "" {
			h.Labels = map[string]string{zoneLabelKey: zone}
		}
		return h
	}
	newClientWith := func(hosts ...*infrav1.PhysicalHost) client.Client {
		b := fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, func(o client.Object) []string {
				return []string{string(o.(*infrav1.PhysicalHost).Status.State)}
			})
		for _, h := range hosts {
			b = b.WithStatusSubresource(h)
		}
		c := b.Build()
		for _, h := range hosts {
			status := h.Status.DeepCopy()
			Expect(c.Create(context.Background(), h)).To(Succeed())
			h.Status = *status
			Expect(c.Status().Update(context.Background(), h)).To(Succeed())
		}
		return c
	}
	machineWith := func(reqs *infrav1.HardwareRequirements) *infrav1.Beskar7Machine {
		return &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "hw-machine", Namespace: "default", Finalizers: []string{Beskar7MachineFinalizer}},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL:   "http://boot/inspect.ipxe",
				TargetImageURL:       "http://boot/kairos.tar.gz",
				TargetImageDigest:    bootTestDigest,
				HardwareRequirements: reqs,
			},
		}
	}
	reconcilerFor := func(c client.Client) *Beskar7MachineReconciler {
		return &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("hw-test")}
	}
	consumerOf := func(c client.Client, name string) bool {
		h := &infrav1.PhysicalHost{}
		Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, h)).To(Succeed())
		return h.Spec.ConsumerRef != nil
	}
	inZone := func(zone string) labels.Selector { return labels.SelectorFromSet(labels.Set{zoneLabelKey: zone}) }

	Describe("checkHardwareRequirements", func() {
		It("passes a report that meets every minimum, and a nil requirement", func() {
			s, errs := checkHardwareRequirements(claimReqs(""), goodReport())
			Expect(s).To(BeNil())
			Expect(errs).To(BeEmpty())
			s, _ = checkHardwareRequirements(nil, nil)
			Expect(s).To(BeNil())
		})

		It("reports the first miss in CPU, memory, disk order with the established messages", func() {
			r := goodReport()
			r.CPUs = []infrav1.CPUInfo{{ID: "0", Cores: 1}, {ID: "1", Cores: 1}}
			s, _ := checkHardwareRequirements(claimReqs(""), r)
			Expect(s.message()).To(Equal("insufficient CPU cores: found 2, required 4"))

			r = goodReport()
			r.Memory = []infrav1.MemoryInfo{{ID: "DIMM0", Capacity: "8GB"}}
			s, _ = checkHardwareRequirements(claimReqs(""), r)
			Expect(s.message()).To(Equal("insufficient memory: found 8 GB, required 16 GB"))

			r = goodReport()
			r.Disks = []infrav1.DiskInfo{{Name: "sda", SizeGB: 40}, {Name: "sdb", SizeGB: 40}}
			s, _ = checkHardwareRequirements(claimReqs(""), r)
			Expect(s.message()).To(Equal("insufficient disk space: found 80 GB, required 100 GB"))
		})

		It("sums the disks, so no single disk has to meet minDiskGB", func() {
			r := goodReport()
			r.Disks = []infrav1.DiskInfo{{Name: "sda", SizeGB: 60}, {Name: "sdb", SizeGB: 60}}
			s, _ := checkHardwareRequirements(claimReqs(""), r)
			Expect(s).To(BeNil())
		})

		It("counts an unparsable memory entry as zero and returns the error", func() {
			r := goodReport()
			r.Memory = []infrav1.MemoryInfo{{ID: "DIMM0", Capacity: "garbage"}}
			s, errs := checkHardwareRequirements(claimReqs(""), r)
			Expect(s).NotTo(BeNil())
			Expect(errs).To(HaveLen(1))
		})
	})

	It("skips a host with no report and a host whose report falls short, and claims one that passes", func() {
		// Names sort so both unsuitable hosts list before the match.
		c := newClientWith(host("a-no-report", "", nil), host("b-short", "", shortReport()), host("c-ok", "", goodReport()))
		r := reconcilerFor(c)

		got, result, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, machineWith(claimReqs(infrav1.HardwareMatchAtClaim)), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Name).To(Equal("c-ok"))
		Expect(result.RequeueAfter).To(Equal(5 * time.Second))
		Expect(consumerOf(c, "a-no-report")).To(BeFalse())
		Expect(consumerOf(c, "b-short")).To(BeFalse())
	})

	It("waits non-terminally with NoMatchingPhysicalHost, naming the constraint and the skip counts", func() {
		c := newClientWith(host("a-no-report", "", nil), host("b-short", "", shortReport()))
		r := reconcilerFor(c)
		b7m := machineWith(claimReqs(infrav1.HardwareMatchAtClaim))

		result, err := r.reconcileNormal(context.Background(), r.Log, b7m, &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: "fake-cluster"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(isTerminallyFailed(b7m)).To(BeFalse(), "a claim-time miss must not be terminal")

		cond := conditions.Get(b7m, infrav1.PhysicalHostAssociatedCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrav1.NoMatchingPhysicalHostReason))
		Expect(cond.Message).To(ContainSubstring("matchAt: Claim"))
		Expect(cond.Message).To(ContainSubstring("minCPUCores=4"))
		Expect(cond.Message).To(ContainSubstring("1 Available PhysicalHost(s) skipped because they have no inspection report"))
		Expect(cond.Message).To(ContainSubstring("1 Available PhysicalHost(s) skipped because their inspection report does not meet hardwareRequirements"))
		Expect(consumerOf(c, "a-no-report")).To(BeFalse())
		Expect(consumerOf(c, "b-short")).To(BeFalse())
	})

	It("names the hardware constraint even when the inventory is empty", func() {
		r := reconcilerFor(newClientWith())
		b7m := machineWith(claimReqs(infrav1.HardwareMatchAtClaim))

		_, err := r.reconcileNormal(context.Background(), r.Log, b7m, &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: "fake-cluster"}})
		Expect(err).NotTo(HaveOccurred())
		cond := conditions.Get(b7m, infrav1.PhysicalHostAssociatedCondition)
		Expect(cond.Reason).To(Equal(infrav1.NoMatchingPhysicalHostReason))
		Expect(cond.Message).To(ContainSubstring("minMemoryGB=16"))
	})

	It("applies the failure domain and the hardware filter together", func() {
		c := newClientWith(
			host("a-rack-2-ok", "rack-2", goodReport()),
			host("b-rack-1-no-report", "rack-1", nil),
			host("c-rack-1-short", "rack-1", shortReport()),
			host("d-rack-1-ok", "rack-1", goodReport()),
		)
		r := reconcilerFor(c)

		got, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, machineWith(claimReqs(infrav1.HardwareMatchAtClaim)), inZone("rack-1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Name).To(Equal("d-rack-1-ok"))
		Expect(consumerOf(c, "a-rack-2-ok")).To(BeFalse(), "a suitable host outside the failure domain is never claimed")
		Expect(consumerOf(c, "b-rack-1-no-report")).To(BeFalse())
		Expect(consumerOf(c, "c-rack-1-short")).To(BeFalse())
	})

	It("claims nothing when the only suitable host is outside the failure domain, and names both constraints", func() {
		c := newClientWith(host("a-rack-2-ok", "rack-2", goodReport()), host("b-rack-1-short", "rack-1", shortReport()))
		r := reconcilerFor(c)
		b7m := machineWith(claimReqs(infrav1.HardwareMatchAtClaim))
		machine := &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: "fake-cluster", FailureDomain: "rack-1"}}

		result, err := r.reconcileNormal(context.Background(), r.Log, b7m, machine)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		cond := conditions.Get(b7m, infrav1.PhysicalHostAssociatedCondition)
		Expect(cond.Reason).To(Equal(infrav1.NoMatchingPhysicalHostReason))
		Expect(cond.Message).To(ContainSubstring(zoneLabelKey + "=rack-1"))
		Expect(cond.Message).To(ContainSubstring("minCPUCores=4"))
		Expect(consumerOf(c, "a-rack-2-ok")).To(BeFalse())
		Expect(consumerOf(c, "b-rack-1-short")).To(BeFalse())
	})

	It("keeps today's behaviour for the default (Inspection) and for an unset matchAt", func() {
		for _, at := range []infrav1.HardwareMatchPoint{infrav1.HardwareMatchAtInspection, ""} {
			c := newClientWith(host("a-no-report", "", nil), host("b-short", "", shortReport()))
			r := reconcilerFor(c)

			got, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, machineWith(claimReqs(at)), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).NotTo(BeNil(), "matchAt %q must not filter at claim", at)
			Expect(got.Name).To(Equal("a-no-report"))
		}

		By("waiting with the unconstrained reason when nothing is available")
		r := reconcilerFor(newClientWith())
		b7m := machineWith(claimReqs(infrav1.HardwareMatchAtInspection))
		_, err := r.reconcileNormal(context.Background(), r.Log, b7m, &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: "fake-cluster"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(conditions.Get(b7m, infrav1.PhysicalHostAssociatedCondition).Reason).To(Equal(infrav1.WaitingForPhysicalHostReason))
	})

	It("does not filter on Claim when no minimum is set", func() {
		c := newClientWith(host("a-no-report", "", nil))
		r := reconcilerFor(c)

		got, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log,
			machineWith(&infrav1.HardwareRequirements{MatchAt: infrav1.HardwareMatchAtClaim}), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
	})

	It("still fails terminally when the claim run's own report misses, whatever matchAt says", func() {
		// The report that passed at claim is replaced by the claim run's fresh
		// one, which falls short: hardware can change between runs.
		r := reconcilerFor(newClientWith())
		b7m := machineWith(claimReqs(infrav1.HardwareMatchAtClaim))
		h := host("a-ok-then-short", "", shortReport())

		result, err := r.validateInspectionReport(context.Background(), r.Log, b7m, h)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(isTerminallyFailed(b7m)).To(BeTrue())
		cond := conditions.Get(b7m, infrav1.InfrastructureReadyCondition)
		Expect(cond.Reason).To(Equal(infrav1.HardwareRequirementsNotMetReason))
		Expect(cond.Message).To(Equal("insufficient CPU cores: found 2, required 4"))
	})

	Describe("the matchAt API field", func() {
		newObj := func(name string, at infrav1.HardwareMatchPoint) *infrav1.Beskar7Machine {
			m := machineWith(&infrav1.HardwareRequirements{MatchAt: at, MinCPUCores: 2})
			m.Name = name
			m.Finalizers = nil
			return m
		}

		It("defaults to Inspection on the API server and keeps an explicit Claim", func() {
			def := newObj("hw-matchat-default", "")
			Expect(k8sClient.Create(ctx, def)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, def))).To(Succeed()) })
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(def), def)).To(Succeed())
			Expect(def.Spec.HardwareRequirements.MatchAt).To(Equal(infrav1.HardwareMatchAtInspection))

			explicit := newObj("hw-matchat-claim", infrav1.HardwareMatchAtClaim)
			Expect(k8sClient.Create(ctx, explicit)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, explicit))).To(Succeed()) })
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(explicit), explicit)).To(Succeed())
			Expect(explicit.Spec.HardwareRequirements.MatchAt).To(Equal(infrav1.HardwareMatchAtClaim))
		})

		It("rejects a value other than Inspection or Claim", func() {
			bad := newObj("hw-matchat-bad", "Sometimes")
			err := k8sClient.Create(ctx, bad)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("matchAt"))
		})
	})
})
