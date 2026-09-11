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
	"errors"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// Cluster API mirrors a Beskar7Machine's Ready summary onto the owning
// Machine's InfrastructureReady condition, and a MachineHealthCheck judges that
// condition by its status and how long it has held it, never by its reason. So
// the machine has to report False, without a break, until its host is
// provisioned: a True in between tells an operator the infrastructure is ready
// while the host is still being inspected, and restarts the MachineHealthCheck's
// clock when the next phase reports False again.

var _ = Describe("Beskar7Machine InfrastructureReady while its host is claimed or being inspected", func() {
	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "inspecting-readiness-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	reachableBMC := func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
		return internalredfish.NewMockClient(), nil
	}

	type phaseCase struct {
		state   string
		phase   infrav1.InspectionPhase
		factory internalredfish.RedfishClientFactory
		// before is the reason InfrastructureReady=False carried into this
		// phase, set ten minutes back; empty means the condition was not set.
		before  string
		wantErr bool
	}

	DescribeTable("the condition the phase leaves behind",
		func(c phaseCase) {
			host := claimedPhysicalHost(ns.Name, "inspected-host", "inspected-machine")
			Expect(k8sClient.Create(ctx, host)).To(Succeed())
			host.Status.State = c.state
			if c.state == infrav1.StateInspecting {
				host.Status.InspectionPhase = c.phase
				host.Status.InspectionTimestamp = ptr.To(metav1.Now())
			}
			if c.phase == infrav1.InspectionPhaseComplete {
				host.Status.InspectionReport = &infrav1.InspectionReport{
					Timestamp: metav1.Now(),
					CPUs:      []infrav1.CPUInfo{{ID: "0", Cores: 8}},
					Memory:    []infrav1.MemoryInfo{{ID: "DIMM0", Capacity: "32GB"}},
					Disks:     []infrav1.DiskInfo{{Name: "sda", SizeGB: 500}},
				}
			}
			Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

			// Associated and with its bootstrap data, as every machine is by the
			// time it gets here. With InfrastructureReady missing, these two alone
			// summarise to Ready=True.
			machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: "inspected-machine", Namespace: ns.Name}}
			setTrue(machine, infrav1.PhysicalHostAssociatedCondition, infrav1.PhysicalHostAssociatedReason)
			setTrue(machine, infrav1.BootstrapDataReadyCondition, infrav1.BootstrapDataReadyReason)
			falseSince := metav1.NewTime(time.Now().Add(-10 * time.Minute).Truncate(time.Second))
			if c.before != "" {
				conditions.Set(machine, metav1.Condition{
					Type: infrav1.InfrastructureReadyCondition, Status: metav1.ConditionFalse,
					Reason: c.before, LastTransitionTime: falseSince,
				})
			}

			r := &Beskar7MachineReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("inspecting-readiness"),
				RedfishClientFactory: c.factory,
			}
			_, err := r.handlePhysicalHostState(ctx, r.Log, machine, host)
			if c.wantErr {
				Expect(err).To(HaveOccurred())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(isTerminallyFailed(machine)).To(BeFalse())

			cond := conditions.Get(machine, infrav1.InfrastructureReadyCondition)
			Expect(cond).NotTo(BeNil(), "InfrastructureReady must be set while the host is %s", c.state)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(infrav1.PhysicalHostNotReadyReason))
			Expect(cond.Message).To(ContainSubstring(host.Name))
			if c.before != "" {
				Expect(cond.LastTransitionTime).To(Equal(falseSince),
					"still not ready, so the MachineHealthCheck's clock keeps running from %s", c.before)
			}

			setReadySummary(machine, r.Log, infrav1.InfrastructureReadyCondition,
				infrav1.PhysicalHostAssociatedCondition, infrav1.BootstrapDataReadyCondition)
			Expect(conditions.IsFalse(machine, clusterv1.ReadyCondition)).To(BeTrue(),
				"the summary Cluster API mirrors onto the Machine must not read Ready while the host is %s", c.state)
		},
		Entry("InUse, booting the inspector on a fresh claim", phaseCase{
			state: infrav1.StateInUse, factory: reachableBMC,
		}),
		Entry("InUse, once the BMC is back from an outage", phaseCase{
			state: infrav1.StateInUse, factory: reachableBMC, before: infrav1.WaitingForBMCReason,
		}),
		Entry("InUse, while the BMC cannot be reached to boot the inspector", phaseCase{
			state: infrav1.StateInUse, factory: failingBMC(errors.New("connection refused")), wantErr: true,
		}),
		Entry("Inspecting, before the report is in", phaseCase{
			state: infrav1.StateInspecting, phase: infrav1.InspectionPhaseBooting, before: infrav1.PhysicalHostNotReadyReason,
		}),
		Entry("Inspecting, once the report is in", phaseCase{
			state: infrav1.StateInspecting, phase: infrav1.InspectionPhaseComplete, before: infrav1.PhysicalHostNotReadyReason,
		}),
	)
})

// The state machine sets InfrastructureReady on every full pass. These passes
// return before it runs, driven through Reconcile against a fake client so the
// early return is certain rather than a matter of timing.
var _ = Describe("Beskar7Machine passes that return before the state machine runs", func() {
	const ns = "early-return"
	machineKey := client.ObjectKey{Namespace: ns, Name: "early-machine"}

	build := func(host *infrav1.PhysicalHost, funcs interceptor.Funcs) (client.Client, *Beskar7MachineReconciler) {
		c := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(
				&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "early-cluster", Namespace: ns}},
				&clusterv1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name: "early-machine", Namespace: ns,
						Labels: map[string]string{clusterv1.ClusterNameLabel: "early-cluster"},
					},
					Spec: clusterv1.MachineSpec{
						ClusterName: "early-cluster",
						Bootstrap:   clusterv1.Bootstrap{DataSecretName: ptr.To("early-bootstrap")},
					},
				},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "early-bootstrap", Namespace: ns}},
				bmcCredentialsSecret(ns),
				&infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
					Name: "early-machine", Namespace: ns,
					Labels:     map[string]string{clusterv1.ClusterNameLabel: "early-cluster"},
					Finalizers: []string{Beskar7MachineFinalizer},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: "early-machine",
					}},
				}},
				host,
			).
			WithStatusSubresource(&infrav1.Beskar7Machine{}, &infrav1.PhysicalHost{}).
			WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, func(o client.Object) []string {
				return []string{o.(*infrav1.PhysicalHost).Status.State}
			}).
			WithInterceptorFuncs(funcs).
			Build()
		return c, &Beskar7MachineReconciler{
			Client: c, Scheme: c.Scheme(),
			Log:                  ctrl.Log.WithName("early-return"),
			BootstrapURLBase:     "https://example.com:8082",
			RedfishClientFactory: failingBMC(errors.New("no pass here reaches the BMC")),
		}
	}

	// reconcile runs the pass that sets the Paused condition, then the one
	// under test.
	reconcile := func(r *Beskar7MachineReconciler) (ctrl.Result, error) {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: machineKey})
		Expect(err).NotTo(HaveOccurred())
		return r.Reconcile(ctx, ctrl.Request{NamespacedName: machineKey})
	}

	expectNotReady := func(c client.Client) {
		got := &infrav1.Beskar7Machine{}
		Expect(c.Get(ctx, machineKey, got)).To(Succeed())
		Expect(conditions.IsTrue(got, infrav1.PhysicalHostAssociatedCondition)).To(BeTrue(),
			"the pass got as far as associating the host")
		infra := conditions.Get(got, infrav1.InfrastructureReadyCondition)
		Expect(infra).NotTo(BeNil(), "a machine holding a host must never be without InfrastructureReady")
		Expect(infra.Status).To(Equal(metav1.ConditionFalse))
		Expect(infra.Reason).To(Equal(infrav1.PhysicalHostNotReadyReason))
		Expect(conditions.IsFalse(got, clusterv1.ReadyCondition)).To(BeTrue(),
			"published Ready: %+v", conditions.Get(got, clusterv1.ReadyCondition))
	}

	It("does not report Ready from the pass that claims a host", func() {
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "free-host", Namespace: ns},
			Spec: infrav1.PhysicalHostSpec{RedfishConnection: infrav1.RedfishConnection{
				Address: "https://mock-redfish.example.invalid:8443", CredentialsSecretRef: "bmc-credentials",
			}},
			Status: infrav1.PhysicalHostStatus{State: infrav1.StateAvailable, Ready: true},
		}
		c, r := build(host, interceptor.Funcs{})

		result, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "a fresh claim is picked up on the next pass")
		claimed := &infrav1.PhysicalHost{}
		Expect(c.Get(ctx, client.ObjectKeyFromObject(host), claimed)).To(Succeed())
		Expect(claimed.Spec.ConsumerRef).NotTo(BeNil())
		expectNotReady(c)
	})

	It("does not report Ready from a pass that fails to signal the bootstrap URL to its host", func() {
		host := claimedPhysicalHost(ns, "held-host", "early-machine")
		host.Status.State = infrav1.StateInUse
		c, r := build(host, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, isHost := obj.(*infrav1.PhysicalHost); isHost {
					return apierrors.NewConflict(infrav1.GroupVersion.WithResource("physicalhosts").GroupResource(),
						obj.GetName(), errors.New("the object has been modified"))
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})

		_, err := reconcile(r)
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "the pass ends on the conflict: %v", err)
		expectNotReady(c)
	})
})

// The specs above drive the controller one pass at a time. This runs a whole
// provisioning under both controllers, with the inspector's two callbacks
// driven through their real handlers, and hands every version of the machine
// the API server published to the assertions. It is also where the machine
// reads itself from a cache that can lag its own last write, which is how an
// early return first published Ready=True here.
var _ = Describe("Beskar7Machine provisioned under a running manager", func() {
	var (
		testNs    *corev1.Namespace
		mgrCancel context.CancelFunc
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "provisioning-readiness-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// envtest never finishes deleting namespaces, so a cluster-wide cache
			// would hand these controllers every other spec's leftovers.
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{testNs.Name: {}}},
			Controller: config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())

		bmc := (&bmcGate{reachable: true}).factory()
		Expect((&PhysicalHostReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("provisioning-readiness-host"),
			Recorder:             record.NewFakeRecorder(100),
			RedfishClientFactory: bmc,
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&Beskar7MachineReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("provisioning-readiness-machine"),
			RedfishClientFactory: bmc,
			BootstrapURLBase:     "https://example.com:8082",
		}).SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, mgrCancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
	})

	AfterEach(func() {
		mgrCancel()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("reports InfrastructureReady=False without a break from its claim until its host is provisioned", func() {
		ns := testNs.Name
		const clusterName = "readiness-cluster"
		log := ctrl.Log.WithName("provisioning-readiness-inspector")

		Expect(k8sClient.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "readiness-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "readiness-machine", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: clusterName,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: "readiness-machine",
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To("readiness-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())

		By("watching the Beskar7Machine from before it exists")
		watching, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		w, err := watching.Watch(ctx, &infrav1.Beskar7MachineList{}, client.InNamespace(ns))
		Expect(err).NotTo(HaveOccurred())
		// Collects up to the first version that reports Ready=True. Stopping the
		// watch afterwards surfaces as an error event of its own, so anything
		// after that version is ignored.
		var (
			mu         sync.Mutex
			published  []*infrav1.Beskar7Machine
			watchFault string
			done       bool
		)
		drained := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(drained)
			for ev := range w.ResultChan() {
				mu.Lock()
				m, isMachine := ev.Object.(*infrav1.Beskar7Machine)
				switch {
				case done:
				case isMachine && ev.Type != watch.Error:
					published = append(published, m)
					done = conditions.IsTrue(m, clusterv1.ReadyCondition)
				default:
					watchFault = fmt.Sprintf("%s event carrying %T: %+v", ev.Type, ev.Object, ev.Object)
					done = true
				}
				mu.Unlock()
			}
		}()

		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "readiness-host", Namespace: ns},
			Spec: infrav1.PhysicalHostSpec{RedfishConnection: infrav1.RedfishConnection{
				Address:              "https://mock-redfish.example.invalid:8443",
				CredentialsSecretRef: "bmc-credentials",
			}},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		hostKey := client.ObjectKeyFromObject(host)
		Expect(k8sClient.Create(ctx, &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "readiness-machine", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
					Name: machine.Name, UID: machine.UID,
				}},
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		})).To(Succeed())

		hostInState := func(state string) func(Gomega) {
			return func(g Gomega) {
				h := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, hostKey, h)).To(Succeed())
				g.Expect(h.Status.State).To(Equal(state))
			}
		}

		By("waiting for the machine to claim the host and boot it into the inspector")
		Eventually(hostInState(infrav1.StateInspecting), 30*time.Second, 100*time.Millisecond).Should(Succeed())

		By("posting the inspection report the way the inspector's callback does")
		Expect((&InspectionHandler{Client: k8sClient, Log: log}).processInspectionReport(ctx, log, ns, host.Name,
			InspectionReportRequest{Manufacturer: "Dell Inc.", CPUs: []CPUData{{ID: "0", Cores: 8}}})).To(Succeed())
		// Until the machine has seen its host deploying, the provisioned
		// callback could move the host past Deploying before the machine ever
		// reconciles it there.
		Eventually(func(g Gomega) {
			hostInState(infrav1.StateDeploying)(g)
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "readiness-machine"}, got)).To(Succeed())
			g.Expect(ptr.Deref(got.Status.Phase, "")).To(Equal("Provisioning"))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())

		By("posting the provisioned callback")
		Expect((&ProvisionedHandler{Client: k8sClient, Log: log}).signalProvisioned(ctx, log, ns, host.Name)).To(Succeed())
		Eventually(func() bool {
			mu.Lock()
			defer mu.Unlock()
			return done
		}, 30*time.Second, 100*time.Millisecond).Should(BeTrue(), "the machine never published Ready=True")
		w.Stop()
		<-drained
		Expect(watchFault).To(BeEmpty(), "the watch must not drop versions")

		By("checking every version published before provisioning completed")
		provisioned := len(published) - 1
		first := published[provisioned]
		Expect(conditions.GetReason(first, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.ProvisionedReason),
			"version %s published Ready=True in phase %q, before its host was provisioned",
			first.ResourceVersion, ptr.Deref(first.Status.Phase, ""))

		var falseSince *metav1.Time
		phases := map[string]bool{}
		for _, m := range published[:provisioned] {
			phases[ptr.Deref(m.Status.Phase, "")] = true
			if c := conditions.Get(m, infrav1.InfrastructureReadyCondition); c != nil {
				Expect(c.Status).To(Equal(metav1.ConditionFalse), "version %s: InfrastructureReady=%s/%s", m.ResourceVersion, c.Status, c.Reason)
				Expect(c.Reason).To(Equal(infrav1.PhysicalHostNotReadyReason), "version %s: %s", m.ResourceVersion, c.Message)
			}
			ready := conditions.Get(m, clusterv1.ReadyCondition)
			if ready == nil {
				continue
			}
			Expect(ready.Status).To(Equal(metav1.ConditionFalse),
				"version %s reported Ready=%s before its host was provisioned: %s", m.ResourceVersion, ready.Status, ready.Message)
			if falseSince == nil {
				falseSince = ready.LastTransitionTime.DeepCopy()
			}
			Expect(ready.LastTransitionTime).To(Equal(*falseSince),
				"version %s restarted the Ready=False clock a MachineHealthCheck times", m.ResourceVersion)
		}
		Expect(falseSince).NotTo(BeNil(), "the machine never published Ready=False before provisioning")
		Expect(phases).To(HaveKey("Inspecting"), "the run went through inspection")
		Expect(phases).To(HaveKey("Provisioning"), "the run went through deployment")
	})
})
