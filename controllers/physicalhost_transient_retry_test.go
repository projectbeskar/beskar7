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
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// refusedConnection builds the error gofish returns when the BMC endpoint
// answers nothing at all — the exact shape seen in CI run 34458472589, where a
// PhysicalHost was created a second after `kubectl rollout status` returned and
// the mock BMC's Service had no programmed endpoint yet.
func refusedConnection(address string) error {
	return refusedConnectionTo(address, "/redfish/v1/")
}

func refusedConnectionTo(address, path string) error {
	return fmt.Errorf("failed to connect to Redfish endpoint %s: %w", address,
		&url.Error{
			Op:  "Get",
			URL: address + path,
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
		})
}

// bmcGate is a Redfish client factory whose reachability the spec controls,
// counting the attempts the controller makes and when it made them.
type bmcGate struct {
	mu        sync.Mutex
	reachable bool
	attempts  []time.Time
}

func (g *bmcGate) factory() internalredfish.RedfishClientFactory {
	return func(_ context.Context, address, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
		g.mu.Lock()
		g.attempts = append(g.attempts, time.Now())
		reachable := g.reachable
		// gofish names the URL that failed, and which one that is depends on how
		// far the handshake got: the service root on one attempt, a collection
		// member on the next. Alternating it here is what makes the "the object
		// is untouched" assertions mean something — the controller's message has
		// to be stable even though the error it came from is not.
		path := "/redfish/v1/"
		if len(g.attempts)%2 == 0 {
			path = "/redfish/v1/Systems"
		}
		g.mu.Unlock()
		if !reachable {
			return nil, refusedConnectionTo(address, path)
		}
		return internalredfish.NewMockClient(), nil
	}
}

func (g *bmcGate) setReachable(v bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reachable = v
}

func (g *bmcGate) attemptCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.attempts)
}

// attemptsWithin counts the attempts made in the d following the first one.
func (g *bmcGate) attemptsWithin(d time.Duration) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.attempts) == 0 {
		return 0
	}
	cutoff := g.attempts[0].Add(d)
	n := 0
	for _, at := range g.attempts {
		if !at.After(cutoff) {
			n++
		}
	}
	return n
}

// A BMC that refuses connections is a fact about the world, not about the
// PhysicalHost: it needs no spec change, and it usually clears on its own.
// These specs pin the retry shape that follows from that — a flat, bounded
// interval driven by the reconciler, rather than the workqueue's exponential
// backoff, which compounded because each failed reconcile's own status write
// re-enqueued the host ahead of the rate-limited retry.
var _ = Describe("PhysicalHost reconcile when the BMC is unreachable", func() {
	// Short enough to watch several retries pass inside a spec.
	const retryInterval = 2 * time.Second

	var (
		testNs     *corev1.Namespace
		host       *infrav1.PhysicalHost
		hostKey    types.NamespacedName
		gate       *bmcGate
		reconciler *PhysicalHostReconciler
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ph-transient-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bmc-credentials", Namespace: testNs.Name},
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("password123"),
			},
		})).To(Succeed())

		host = &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "flaky-bmc-host", Namespace: testNs.Name},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://mock-redfish.example.invalid:8443",
					CredentialsSecretRef: "bmc-credentials",
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		hostKey = types.NamespacedName{Name: host.Name, Namespace: host.Namespace}

		gate = &bmcGate{reachable: false}
		reconciler = &PhysicalHostReconciler{
			Client:                 k8sClient,
			Scheme:                 k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("ph-transient-test"),
			Recorder:               record.NewFakeRecorder(100),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: retryInterval,
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	// reconcileOnce runs one pass; the first pass only adds the finalizer.
	reconcileOnce := func() (ctrl.Result, error) {
		return reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
	}

	It("Should retry on a flat interval with no error, so the workqueue backoff cannot compound", func() {
		By("Reconciling once to add the finalizer")
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())

		By("Reconciling against the unreachable BMC")
		result, err := reconcileOnce()

		// The error is deliberately not returned. Returning it hands the retry
		// to the workqueue's exponential rate-limiter, which counts this
		// reconcile as a failure and doubles the next delay — while the status
		// write below re-enqueues the host immediately anyway. That combination
		// is what produced ~20 reconciles in one second and then minutes of
		// silence.
		Expect(err).NotTo(HaveOccurred(), "a network-level BMC failure must not be returned as a reconcile error")
		Expect(result.RequeueAfter).To(Equal(retryInterval), "the retry cadence must be the flat transient interval")

		By("Checking the host is parked in Error with the connection condition set")
		fetched := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, fetched)).To(Succeed())
		Expect(fetched.Status.State).To(Equal(infrav1.StateError))
		Expect(fetched.Status.Ready).To(BeFalse())
		Expect(fetched.Status.ErrorMessage).To(ContainSubstring("BMC unreachable"))
		Expect(fetched.Status.ErrorMessage).To(ContainSubstring("connection refused"))

		cond := conditions.Get(fetched, infrav1.RedfishConnectionReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrav1.BMCUnreachableReason),
			"the reason is how the Beskar7Machine controller tells a BMC outage from a failure that needs a change")
		Expect(cond.Message).To(ContainSubstring("BMC unreachable"))
	})

	It("Should not re-enqueue itself: a repeated failure leaves the object untouched", func() {
		// The mechanical reason the burst is gone. A reconcile that writes to
		// the host it just failed on generates a watch event for itself, and
		// controller-runtime's priority queue puts that event ahead of any
		// pending delayed retry — so the retry interval never gets a chance to
		// elapse. Keeping the failure message stable (a summary of the failure
		// class, not the raw per-attempt error text, which names whichever URL
		// failed) makes the second write a no-op, and a no-op write is not an
		// event.
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())

		afterFirstFailure := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, afterFirstFailure)).To(Succeed())

		for i := 0; i < 3; i++ {
			result, rerr := reconcileOnce()
			Expect(rerr).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(retryInterval))
		}

		afterMoreFailures := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, afterMoreFailures)).To(Succeed())
		Expect(afterMoreFailures.ResourceVersion).To(Equal(afterFirstFailure.ResourceVersion),
			"repeating the same failure must not write to the host; every write is a watch event that would trigger the next attempt at once")

		By("Checking the status message does not carry the per-attempt URL that made it churn")
		Expect(afterMoreFailures.Status.ErrorMessage).NotTo(ContainSubstring("/redfish/v1/"))
	})

	It("Should recover to Available on the first attempt after the BMC answers", func() {
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())

		errored := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, errored)).To(Succeed())
		Expect(errored.Status.State).To(Equal(infrav1.StateError))

		By("Letting the BMC answer again")
		gate.setReachable(true)

		result, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(5*time.Minute), "a healthy host falls back to the steady-state resync")

		recovered := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, recovered)).To(Succeed())
		Expect(recovered.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(recovered.Status.Ready).To(BeTrue())
		Expect(recovered.Status.ErrorMessage).To(BeEmpty())
		Expect(conditions.IsTrue(recovered, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
	})

	It("Should treat a mid-session connection drop the same way", func() {
		// The service root answered and then the next query found nothing
		// listening — the second failure mode in the CI run, where the ClusterIP
		// still pointed at a pod that was going away.
		dropping := internalredfish.NewMockClient()
		dropping.ShouldFail["GetSystemInfo"] = fmt.Errorf("failed to retrieve systems: %w",
			refusedConnection("https://mock-redfish.example.invalid:8443"))
		reconciler.RedfishClientFactory = func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
			return dropping, nil
		}

		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())

		result, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred(), "a dropped connection mid-session is the same network failure as a refused one")
		Expect(result.RequeueAfter).To(Equal(retryInterval))

		fetched := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, fetched)).To(Succeed())
		Expect(fetched.Status.State).To(Equal(infrav1.StateError))
		Expect(fetched.Status.ErrorMessage).To(ContainSubstring("BMC unreachable"))
	})

	It("Should still return an error for a Redfish failure that needs something to change", func() {
		// A BMC that answers but has no ComputerSystem is a configuration or
		// firmware problem: retrying it every few seconds forever would be
		// noise, so it keeps the workqueue's exponential backoff.
		noSystems := internalredfish.NewMockClient()
		noSystems.ShouldFail["GetSystemInfo"] = fmt.Errorf("no systems found")
		reconciler.RedfishClientFactory = func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
			return noSystems, nil
		}

		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())

		result, err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no systems found"))
		Expect(result.RequeueAfter).To(BeZero(), "the rate-limiter governs this retry, not a fixed interval")

		fetched := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, fetched)).To(Succeed())
		Expect(fetched.Status.State).To(Equal(infrav1.StateError))
		cond := conditions.Get(fetched, infrav1.RedfishConnectionReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(infrav1.RedfishQueryFailedReason))
	})

	It("Should default the retry interval when the field is unset", func() {
		Expect((&PhysicalHostReconciler{}).transientRetryInterval()).To(Equal(DefaultTransientRetryInterval))
		Expect(DefaultTransientRetryInterval).To(And(
			BeNumerically(">=", 10*time.Second),
			BeNumerically("<=", 30*time.Second),
		), "short enough to re-enrol promptly, long enough not to hammer a rebooting BMC")
	})
})

// The specs above drive Reconcile by hand. This one lets the controller run
// under a manager, which is the only way to see what the workqueue actually
// does with the results: that the retries keep coming on the timer, and that
// the host recovers without anything poking it.
var _ = Describe("PhysicalHost transient-failure retries under a running manager", func() {
	const retryInterval = 2 * time.Second

	var (
		testNs    *corev1.Namespace
		hostKey   types.NamespacedName
		gate      *bmcGate
		mgrCtx    context.Context
		mgrCancel context.CancelFunc
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ph-transient-mgr-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bmc-credentials", Namespace: testNs.Name},
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("password123"),
			},
		})).To(Succeed())

		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// Envtest has no namespace controller, so every host the other specs
			// created is still there, in a namespace that will never finish
			// deleting. A cluster-wide cache would hand them all to this
			// controller and its attempt counter would be measuring them too.
			Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{testNs.Name: {}}},
			// Several specs in this package each build a manager and register a
			// controller of the same name; controller-runtime's metric registry
			// is global and would reject the second one.
			Controller: config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())

		gate = &bmcGate{reachable: false}
		Expect((&PhysicalHostReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    ctrl.Log.WithName("ph-transient-mgr-test"),
			// A fake recorder, not mgr.GetEventRecorderFor: that one is deprecated,
			// and nothing here asserts on events.
			Recorder:               record.NewFakeRecorder(100),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: retryInterval,
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "flaky-bmc-host", Namespace: testNs.Name},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://mock-redfish.example.invalid:8443",
					CredentialsSecretRef: "bmc-credentials",
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		hostKey = types.NamespacedName{Name: host.Name, Namespace: host.Namespace}
	})

	AfterEach(func() {
		mgrCancel()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("Should keep retrying on the interval instead of bursting, and recover once the BMC answers", func() {
		By("Waiting for the host to be parked in Error")
		Eventually(func(g Gomega) {
			fetched := &infrav1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, fetched)).To(Succeed())
			g.Expect(fetched.Status.State).To(Equal(infrav1.StateError))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("Letting a few retry intervals pass")
		time.Sleep(3 * retryInterval)

		// Before the fix this was ~20 attempts inside the first second: every
		// failed reconcile wrote a new message, the write woke the controller,
		// and the attempt failed again. The bound here is deliberately loose —
		// it is two orders of magnitude away from the burst, and the point is
		// the shape, not an exact count.
		burst := gate.attemptsWithin(time.Second)
		Expect(burst).To(BeNumerically("<=", 5),
			"a failed attempt must not immediately trigger the next one")

		total := gate.attemptCount()
		Expect(total).To(BeNumerically(">=", 2), "the retries must keep coming; a swallowed requeue would stop them")
		Expect(total).To(BeNumerically("<=", 12), "and they must stay on the interval")

		By("Letting the BMC answer again and waiting for the host to enrol")
		gate.setReachable(true)
		before := gate.attemptCount()

		// Bounded by the retry interval, not by the five-minute steady-state
		// resync and not by a backoff that grew while the BMC was down.
		Eventually(func(g Gomega) {
			fetched := &infrav1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, fetched)).To(Succeed())
			g.Expect(fetched.Status.State).To(Equal(infrav1.StateAvailable))
			g.Expect(fetched.Status.Ready).To(BeTrue())
		}, 4*retryInterval, 200*time.Millisecond).Should(Succeed())

		Expect(gate.attemptCount()).To(BeNumerically("<=", before+3),
			"recovery must take the next scheduled attempt, not a rebuilt backoff")
	})
})

// The retry above leaves a host cycling Error -> Available while Beskar7Machines
// may be looking for one to claim. Two things keep that from going wrong, and
// both are asserted here: an Error host is not in the Available field index the
// claim lists through, and the claim patch carries an optimistic lock, so a
// claim built on a cached copy that has since changed is rejected rather than
// applied to a host in a state the machine never saw.
var _ = Describe("Beskar7Machine claim path against a host recovering from Error", func() {
	newClientWithHost := func(host *infrav1.PhysicalHost, machine *infrav1.Beskar7Machine) client.Client {
		c := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(machine).
			WithStatusSubresource(host).
			WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, func(obj client.Object) []string {
				h, ok := obj.(*infrav1.PhysicalHost)
				if !ok {
					return nil
				}
				return []string{string(h.Status.State)}
			}).
			Build()
		Expect(c.Create(context.Background(), host)).To(Succeed())
		Expect(c.Status().Update(context.Background(), host)).To(Succeed())
		return c
	}

	It("Should leave a host in Error unclaimed, and claim it once it recovers to Available", func() {
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "recovering-host", Namespace: "default"},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://mock-redfish.example.invalid:8443",
					CredentialsSecretRef: "bmc-credentials",
				},
			},
			Status: infrav1.PhysicalHostStatus{
				State:        infrav1.StateError,
				ErrorMessage: "BMC unreachable (connection refused); retrying every 15s",
			},
		}
		machine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "waiting-machine", Namespace: "default"},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		c := newClientWithHost(host, machine)
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("recovery-claim-test")}

		By("Not claiming the host while its BMC is unreachable")
		got, result, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, machine, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil(), "a host in Error is not in the Available index and must not be claimed")
		Expect(result).To(Equal(ctrl.Result{}))

		fetched := &infrav1.PhysicalHost{}
		Expect(c.Get(context.Background(), client.ObjectKeyFromObject(host), fetched)).To(Succeed())
		Expect(fetched.Spec.ConsumerRef).To(BeNil())

		By("Claiming it as soon as the BMC answers and the host is Available again")
		fetched.Status.State = infrav1.StateAvailable
		fetched.Status.Ready = true
		fetched.Status.ErrorMessage = ""
		Expect(c.Status().Update(context.Background(), fetched)).To(Succeed())

		got, result, err = r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, machine, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil(), "a recovered host must be claimable on the very next pass")
		Expect(got.Name).To(Equal(host.Name))
		Expect(result.RequeueAfter).To(Equal(5 * time.Second))

		claimed := &infrav1.PhysicalHost{}
		Expect(c.Get(context.Background(), client.ObjectKeyFromObject(host), claimed)).To(Succeed())
		Expect(claimed.Spec.ConsumerRef).NotTo(BeNil())
		Expect(claimed.Spec.ConsumerRef.Name).To(Equal(machine.Name))
	})

	It("Should reject a claim built on a copy of the host that has since changed", func() {
		// The claim lists through a cache, so the copy it patches can be one
		// version behind the host that just flipped Error -> Available (or back).
		// MergeFromWithOptimisticLock is what turns that into a Conflict the
		// caller retries, instead of a claim written over a state the machine
		// never saw.
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "moving-host", Namespace: "default"},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://mock-redfish.example.invalid:8443",
					CredentialsSecretRef: "bmc-credentials",
				},
			},
			Status: infrav1.PhysicalHostStatus{State: infrav1.StateAvailable, Ready: true},
		}
		machine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "racing-machine", Namespace: "default"},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		c := newClientWithHost(host, machine)

		By("Taking a copy of the host, as the cached List would")
		stale := &infrav1.PhysicalHost{}
		Expect(c.Get(context.Background(), client.ObjectKeyFromObject(host), stale)).To(Succeed())

		By("Letting the host move on: its BMC stopped answering")
		current := &infrav1.PhysicalHost{}
		Expect(c.Get(context.Background(), client.ObjectKeyFromObject(host), current)).To(Succeed())
		current.Status.State = infrav1.StateError
		current.Status.Ready = false
		current.Status.ErrorMessage = "BMC unreachable (connection refused); retrying every 15s"
		Expect(c.Status().Update(context.Background(), current)).To(Succeed())

		By("Claiming the stale copy the way findAndClaimOrGetAssociatedHost does")
		base := stale.DeepCopy()
		stale.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind:       "Beskar7Machine",
			APIVersion: InfrastructureAPIVersion,
			Name:       machine.Name,
			Namespace:  machine.Namespace,
		}
		err := c.Patch(context.Background(), stale,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "a stale claim must be rejected as a Conflict, which the caller retries")

		unclaimed := &infrav1.PhysicalHost{}
		Expect(c.Get(context.Background(), client.ObjectKeyFromObject(host), unclaimed)).To(Succeed())
		Expect(unclaimed.Spec.ConsumerRef).To(BeNil())
	})
})
