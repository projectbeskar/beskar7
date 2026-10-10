/*
Copyright 2026 The Beskar7 Authors.

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
	"github.com/stmcginnis/gofish/schemas"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// The provisioning-readiness spec failed one run in twelve with the host stuck
// in Inspecting. Its log showed a second machine pass, reading a host its cache
// still held at InUse, run triggerInspection again. Its inspect write conflicted
// with the host's own, and the retry read the host afresh, saw it still claimed
// by this machine, and wrote inspect onto a host that was already Inspecting.
// The host reconciler applied that duplicate after the inspection report had
// arrived, which set InspectionPhase back to Booting; the machine, validating
// the report, found the inspection no longer complete and declined to send
// inspect-complete. It then waited for a report that had already come, which in
// production ends in InspectionTimedOut after ten minutes.
//
// Both halves are closed. The host applies an inspect only while it is InUse,
// and the machine's retry stops once its fresh read shows the host has left
// InUse. These specs pin each half on its own, and replay the whole race.

var _ = Describe("A duplicate inspect request", func() {
	const callbackBase = "https://callback.example.com:8082"

	var (
		ns             *corev1.Namespace
		gate           *bmcGate
		hostReconciler *PhysicalHostReconciler
		hostLogs       *conflictLogs
		logs           *conflictLogs
		writes         *conflictWrites
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "duplicate-inspect-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		gate = &bmcGate{reachable: true}
		hostLogs = newConflictLogs()
		hostReconciler = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    hostLogs.logger(),
			Recorder:               record.NewFakeRecorder(100),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: time.Second,
		}
		logs = newConflictLogs()
		writes = &conflictWrites{}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	report := func() *infrav1.InspectionReport {
		return buildInspectionReport(InspectionReportRequest{
			Manufacturer: "Acme", Model: "Fast-1000", CPUs: []CPUData{{ID: "cpu0", Cores: 8}},
		})
	}

	// machineOver returns the machine controller reading and writing through c,
	// with a log that keeps everything, over a BMC that answers.
	machineOver := func(c client.Client, factory internalredfish.RedfishClientFactory) *Beskar7MachineReconciler {
		return &Beskar7MachineReconciler{
			Client: c, Scheme: k8sClient.Scheme(),
			Log:                  logs.logger(),
			RedfishClientFactory: factory,
			BootstrapURLBase:     callbackBase,
		}
	}

	// inUseHost stages a claimed host that has just been taken to InUse, which
	// is where its machine finds it before it boots it: no run state yet, a
	// healthy BMC connection. The credentials a claim mints are created when the
	// machine's triggerInspection mints them.
	inUseHost := func() (client.ObjectKey, *infrav1.Beskar7Machine, *clusterv1.Machine) {
		b7m, machine := consumerWithBootstrapData(ns.Name, "duplicate-machine", "DUPLICATE-BOOTSTRAP-DATA")
		b7m.Finalizers = []string{Beskar7MachineFinalizer}
		key := provisioningHost(ns.Name, "duplicate-host", b7m.Name, infrav1.StateInUse, nil)
		host := getPhysicalHost(key)
		host.Status.InspectionTimestamp = nil
		host.Status.InspectionPhase = ""
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
		return key, b7m, machine
	}

	// beginInspection takes an InUse host to Inspecting the way a run does: the
	// machine boots it and writes inspect, and the host reconciler applies it.
	beginInspection := func(r *Beskar7MachineReconciler, key client.ObjectKey, b7m *infrav1.Beskar7Machine) {
		_, err := r.triggerInspection(ctx, r.Log, b7m, getPhysicalHost(key))
		Expect(err).NotTo(HaveOccurred())
		Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))
		inspecting := settlePhysicalHost(hostReconciler, key)
		Expect(inspecting.Status.State).To(Equal(infrav1.StateInspecting))
		Expect(inspecting.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseBooting))
		Expect(inspecting.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
	}

	// receiveReport posts the inspector's report and lets the host apply it.
	receiveReport := func(key client.ObjectKey) {
		postInspectionReport(key)
		received := settlePhysicalHost(hostReconciler, key)
		Expect(received.Status.State).To(Equal(infrav1.StateInspecting))
		Expect(received.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete))
		Expect(received.Status.InspectionReport).NotTo(BeNil())
	}

	expectNoRequest := func(key client.ObjectKey) {
		Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
		Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(callbackBindingAnnotation(InspectionRequestAnnotation)))
	}

	// expectNothingSecretLogged checks both logs for the credentials and for
	// every binding the machine computed.
	expectNothingSecretLogged := func(key client.ObjectKey) {
		for _, text := range []string{logs.text(), hostLogs.text()} {
			Expect(text).NotTo(ContainSubstring(callbackTokenOf(key)), "the bearer token is never logged")
			for _, binding := range writes.bindings() {
				Expect(text).NotTo(ContainSubstring(binding), "a binding is never logged")
			}
		}
	}

	Describe("reaching the PhysicalHost reconciler", func() {
		DescribeTable("is removed, and nothing else changes, on a host that is past InUse",
			func(state string, withReport bool) {
				key := provisioningHost(ns.Name, "past-inuse-host", "past-inuse-machine", state, nil)
				host := getPhysicalHost(key)
				if withReport {
					host.Status.InspectionPhase = infrav1.InspectionPhaseComplete
					host.Status.InspectionReport = report()
					setTrue(host, infrav1.HostInspectedCondition, infrav1.HostInspectedReason)
					Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
				}
				before := getPhysicalHost(key)
				requestInspection(key, "inspect")
				Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))

				after := settlePhysicalHost(hostReconciler, key)

				Expect(after.Status.State).To(Equal(state), "a duplicate inspect never moves the host back")
				Expect(after.Status.InspectionPhase).To(Equal(before.Status.InspectionPhase))
				Expect(after.Status.InspectionTimestamp).To(Equal(before.Status.InspectionTimestamp))
				Expect(after.Status.DeployingTimestamp).To(Equal(before.Status.DeployingTimestamp))
				Expect(conditions.IsTrue(after, infrav1.HostInspectedCondition)).To(Equal(conditions.IsTrue(before, infrav1.HostInspectedCondition)))
				Expect(apiequality.Semantic.DeepEqual(after.Status.InspectionReport, before.Status.InspectionReport)).To(BeTrue(),
					"the report the host received is intact")
				expectNoRequest(key)

				ignored := hostLogs.with("Ignoring inspection-request annotation")
				Expect(ignored).To(HaveLen(1))
				Expect(ignored[0].level).To(BeZero(), "logged at Info")
				Expect(ignored[0].kv).To(ContainElements("inspect", state), "the log names the value and the state")
				Expect(hostLogs.text()).NotTo(ContainSubstring(callbackTokenOf(key)))
			},
			Entry("Inspecting, the report received (the readiness flake)", infrav1.StateInspecting, true),
			Entry("Inspecting, the report not received yet", infrav1.StateInspecting, false),
			Entry("Deploying", infrav1.StateDeploying, true),
		)

		It("leaves the report the host has received for the machine to validate", func() {
			key, _, _ := inUseHost()
			requestInspection(key, "inspect")
			settlePhysicalHost(hostReconciler, key)
			receiveReport(key)
			received := getPhysicalHost(key)

			requestInspection(key, "inspect")
			after := settlePhysicalHost(hostReconciler, key)

			Expect(after.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete))
			Expect(apiequality.Semantic.DeepEqual(after.Status.InspectionReport, received.Status.InspectionReport)).To(BeTrue())
			Expect(after.Status.State).To(Equal(infrav1.StateInspecting))
			expectNoRequest(key)
		})

		It("still starts an inspection on a host that is InUse", func() {
			key, _, _ := inUseHost()
			requestInspection(key, "inspect")

			after := settlePhysicalHost(hostReconciler, key)

			Expect(after.Status.State).To(Equal(infrav1.StateInspecting))
			Expect(after.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseBooting))
			Expect(after.Status.InspectionTimestamp).NotTo(BeNil())
			expectNoRequest(key)
			Expect(hostLogs.with("Ignoring inspection-request annotation")).To(BeEmpty())
		})

		It("is dropped when it lands together with the claim, before the host is InUse, and the machine's next one applies", func() {
			// The machine sends inspect only to a host it has read InUse, so a
			// host that is still Available when it finds one has not been seen
			// by its machine yet.
			host := claimedPhysicalHost(ns.Name, "claim-race-host", "claim-race-machine")
			host.Spec.ConsumerRef = nil
			host.Finalizers = []string{PhysicalHostFinalizer}
			Expect(k8sClient.Create(ctx, host)).To(Succeed())
			key := client.ObjectKeyFromObject(host)
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateAvailable))

			claimWithRequest(key, "claim-race-machine", "inspect")
			claimed := settlePhysicalHost(hostReconciler, key)

			Expect(claimed.Status.State).To(Equal(infrav1.StateInUse), "the claim takes the host to InUse, and nothing starts an inspection yet")
			Expect(claimed.Status.InspectionPhase).To(BeEmpty())
			Expect(claimed.Status.InspectionTimestamp).To(BeNil())
			expectNoRequest(key)

			requestInspection(key, "inspect")
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateInspecting))
		})

		It("is dropped on a host in an Error about its BMC, and the machine sends it again once the host is back at InUse", func() {
			key, b7m, _ := inUseHost()
			mockRf := internalredfish.NewMockClient()
			r := machineOver(k8sClient, func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return mockRf, nil
			})
			staleInUse := getPhysicalHost(key)

			By("the BMC going away: the host is published in Error")
			gate.setReachable(false)
			errored := settlePhysicalHost(hostReconciler, key)
			Expect(errored.Status.State).To(Equal(infrav1.StateError))
			Expect(conditions.GetReason(errored, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.BMCUnreachableReason))
			Expect(hostWaitingForBMC(errored)).To(BeTrue(), "its machine waits for the BMC instead of failing")

			By("the machine, which read the host InUse before that, boots it; its write meets the host's and is not retried onto an Error host")
			_, err := r.triggerInspection(ctx, r.Log, b7m, staleInUse)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockRf.SetPowerStateCalled).To(BeTrue())
			Expect(mockRf.PowerState).To(Equal(schemas.OnPowerState))
			expectNoRequest(key)
			Expect(logs.with("no longer applies")).To(HaveLen(1))

			By("an inspect that does reach the host in Error, as when the machine's write lands ahead of the host's status patch")
			requestInspection(key, "inspect")
			stillErrored := settlePhysicalHost(hostReconciler, key)
			Expect(stillErrored.Status.State).To(Equal(infrav1.StateError), "the request did not take the host out of Error")
			Expect(stillErrored.Status.InspectionPhase).To(BeEmpty())
			Expect(stillErrored.Status.InspectionTimestamp).To(BeNil())
			expectNoRequest(key)
			ignored := hostLogs.with("Ignoring inspection-request annotation")
			Expect(ignored).To(HaveLen(1))
			Expect(ignored[0].kv).To(ContainElements("inspect", infrav1.StateError))

			By("the BMC coming back: the host is InUse again")
			gate.setReachable(true)
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateInUse))

			By("the machine sending inspect again, without restarting the host it booted for this claim")
			_, err = r.triggerInspection(ctx, r.Log, b7m, getPhysicalHost(key))
			Expect(err).NotTo(HaveOccurred())
			Expect(mockRf.ResetCalled).To(BeFalse(), "a restart would interrupt the inspector booting (bootedForClaim)")
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))
			Expect(callbackBinderFor(key).holds(getPhysicalHost(key), InspectionRequestAnnotation, "")).To(BeTrue())

			inspecting := settlePhysicalHost(hostReconciler, key)
			Expect(inspecting.Status.State).To(Equal(infrav1.StateInspecting))
			Expect(inspecting.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseBooting))
			expectNoRequest(key)
			Expect(logs.errors()).To(BeEmpty())
		})
	})

	Describe("sent by the machine's retry", func() {
		It("is not written when the first write conflicts and the fresh read shows the host already Inspecting; the run goes on", func() {
			key, b7m, machine := inUseHost()
			var appliedAt *metav1.Time
			writes.conflicts = 1
			writes.otherWriter = func(int) {
				// The earlier pass's inspect, applied by the host reconciler.
				requestInspection(key, "inspect")
				appliedAt = settlePhysicalHost(hostReconciler, key).Status.InspectionTimestamp
			}
			r := machineOver(writes.client(key), reachableBMC())

			result, err := r.triggerInspection(ctx, r.Log, b7m, getPhysicalHost(key))

			Expect(err).NotTo(HaveOccurred(), "an inspection that has begun is not an error")
			Expect(appliedAt).NotTo(BeNil(), "the earlier inspect must be applied under the write for this spec to mean anything")
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(ptr.Deref(b7m.Status.Phase, "")).To(Equal("Inspecting"))
			Expect(writes.attempts()).To(Equal(1), "only the write that lost the race; no second inspect")
			expectNoRequest(key)
			inspecting := getPhysicalHost(key)
			Expect(inspecting.Status.State).To(Equal(infrav1.StateInspecting))
			Expect(inspecting.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseBooting))
			Expect(inspecting.Status.InspectionTimestamp).To(Equal(appliedAt))
			Expect(logs.errors()).To(BeEmpty())

			stopped := logs.with("no longer applies")
			Expect(stopped).To(HaveLen(1))
			Expect(stopped[0].level).To(Equal(1), "stopping is logged at V(1)")
			Expect(stopped[0].kv).To(ContainElements(key.Name, infrav1.StateInspecting))
			Expect(logs.with("Inspection boot triggered successfully")).To(BeEmpty(), "nothing was written, so nothing was triggered")
			told := logs.with("no inspection request was written")
			Expect(told).To(HaveLen(1))
			Expect(told[0].level).To(BeZero(), "what happened is logged at Info")
			Expect(told[0].kv).To(ContainElement(infrav1.StateInspecting))

			By("the report arriving: the machine validates it and the host goes to Deploying")
			receiveReport(key)
			writes.mu.Lock()
			writes.conflicts = 0
			writes.mu.Unlock()
			result, err = r.reconcileNormal(ctx, r.Log, b7m, machine)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueShortly))
			Expect(isTerminallyFailed(b7m)).To(BeFalse())
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"))
			deploying := settlePhysicalHost(hostReconciler, key)
			Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying))
			Expect(deploying.Status.DeployingTimestamp).NotTo(BeNil())
			expectNothingSecretLogged(key)
		})

		for _, left := range []struct {
			name  string
			state string
		}{
			{"Deploying", infrav1.StateDeploying},
			{"Error", infrav1.StateError},
		} {
			left := left
			It("is not written when the fresh read shows the host "+left.name+": no second write, no error", func() {
				key, b7m, _ := inUseHost()
				var raced bool
				writes.conflicts = 1
				writes.otherWriter = func(int) {
					raced = true
					host := getPhysicalHost(key)
					host.Status.State = left.state
					Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
				}
				r := machineOver(writes.client(key), reachableBMC())

				result, err := r.triggerInspection(ctx, r.Log, b7m, getPhysicalHost(key))

				Expect(err).NotTo(HaveOccurred())
				Expect(raced).To(BeTrue())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(writes.attempts()).To(Equal(1))
				expectNoRequest(key)
				Expect(getPhysicalHost(key).Status.State).To(Equal(left.state))
				stopped := logs.with("no longer applies")
				Expect(stopped).To(HaveLen(1))
				Expect(stopped[0].level).To(Equal(1))
				Expect(stopped[0].kv).To(ContainElement(left.state))
				Expect(logs.with("Inspection boot triggered successfully")).To(BeEmpty())
				Expect(logs.errors()).To(BeEmpty())
			})
		}

		It("still writes, signed with the credentials it just minted, when the conflict was only the host's own write and the host is still InUse", func() {
			key, b7m, _ := inUseHost()
			writes.conflicts = 1
			r := machineOver(writes.client(key), reachableBMC())

			_, err := r.triggerInspection(ctx, r.Log, b7m, getPhysicalHost(key))

			Expect(err).NotTo(HaveOccurred())
			Expect(writes.attempts()).To(Equal(2), "one write lost the race, the retry won it")
			landed := getPhysicalHost(key)
			Expect(landed.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))
			Expect(callbackBinderFor(key).holds(landed, InspectionRequestAnnotation, "")).To(BeTrue())
			Expect(logs.with("no longer applies")).To(BeEmpty())
			Expect(logs.with("Inspection boot triggered successfully")).To(HaveLen(1))
			Expect(logs.with("no inspection request was written")).To(BeEmpty())
			expectNothingSecretLogged(key)
		})

		It("still fails the pass when the host is no longer claimed by this machine", func() {
			key, b7m, _ := inUseHost()
			var raced bool
			writes.conflicts = 1
			writes.otherWriter = func(int) {
				raced = true
				releasePhysicalHost(key)
			}
			r := machineOver(writes.client(key), reachableBMC())

			_, err := r.triggerInspection(ctx, r.Log, b7m, getPhysicalHost(key))

			Expect(raced).To(BeTrue())
			Expect(err).To(MatchError(ContainSubstring("no longer claimed by this machine")))
			Expect(writes.attempts()).To(Equal(1))
			expectNoRequest(key)
		})
	})

	Describe("the whole race", func() {
		// The machine's second pass reads a host its cache still holds at InUse,
		// and its retry reaches the host after the inspection report has been
		// applied. Nothing may take the host back out of Complete.
		It("replays a stale second machine pass after the report: the host still reaches Deploying", func() {
			key, b7m, machine := inUseHost()
			r := machineOver(k8sClient, reachableBMC())
			stale := getPhysicalHost(key)

			beginInspection(r, key, b7m)
			receiveReport(key)

			By("the second pass, working from the host as it was at InUse: its write conflicts for real")
			_, err := r.triggerInspection(ctx, r.Log, b7m, stale)
			Expect(err).NotTo(HaveOccurred())
			Expect(logs.errors()).To(BeEmpty())

			By("the host reconciler's next pass, which would have applied a duplicate")
			settled := settlePhysicalHost(hostReconciler, key)
			Expect(settled.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete),
				"the report the host received must not be wiped by a request for an inspection already under way")
			Expect(settled.Status.State).To(Equal(infrav1.StateInspecting))
			expectNoRequest(key)

			By("the machine validating the report and the host going to Deploying")
			result, err := r.reconcileNormal(ctx, r.Log, b7m, machine)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueShortly))
			Expect(isTerminallyFailed(b7m)).To(BeFalse())
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"),
				"the machine was not left waiting for a report that had already come")
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateDeploying))
		})

		It("keeps the report when a duplicate is applied between the machine's validation and its inspect-complete write", func() {
			key, b7m, machine := inUseHost()
			r := machineOver(writes.client(key), reachableBMC())
			beginInspection(r, key, b7m)
			receiveReport(key)
			received := getPhysicalHost(key).Status.InspectionReport

			// The inspect-complete write loses the race to the host reconciler
			// applying the stale duplicate. The count is of every request write
			// since the spec began, beginInspection's among them.
			var raced bool
			already := writes.attempts()
			writes.conflicts = already + 1
			writes.otherWriter = func(int) {
				raced = true
				requestInspection(key, "inspect")
				settlePhysicalHost(hostReconciler, key)
			}

			result, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

			Expect(err).NotTo(HaveOccurred())
			Expect(raced).To(BeTrue(), "the duplicate must land under the write for this spec to mean anything")
			Expect(result.RequeueAfter).To(Equal(requeueShortly))
			Expect(isTerminallyFailed(b7m)).To(BeFalse())
			Expect(logs.with("no longer applies")).To(BeEmpty(), "the inspection stayed complete, so the retry had every reason to write")
			Expect(writes.attempts()).To(Equal(already+2), "one write lost the race, the retry won it")
			landed := getPhysicalHost(key)
			Expect(landed.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"))
			Expect(apiequality.Semantic.DeepEqual(landed.Status.InspectionReport, received)).To(BeTrue())

			deploying := settlePhysicalHost(hostReconciler, key)
			Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying))
			Expect(deploying.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete))
			expectNothingSecretLogged(key)
		})
	})
})
