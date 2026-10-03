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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/schemas"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// The Beskar7Machine controller drives a provisioning run through three values
// of the inspection-request annotation: inspect, inspect-complete and timeout.
// Anyone allowed to patch PhysicalHosts could write them by hand, and fail a
// run at once (timeout) or take a host to Deploying (inspect-complete) with no
// machine behind it. Each value now carries a binding like the inspector's
// callbacks do, and the reconciler acts only on one it can recompute from the
// host's bootstrap-token Secret (D-037, the scheme of D-034). The specs that run
// all four signals through the same table are in callback_binding_test.go; these
// are the ones particular to the machine's requests: how it signs, what the
// binding covers, how the reconciler reads the Secret, and what an upgrade does
// to a request in flight.

// requestMachine returns the Beskar7Machine that holds host, as the machine
// controller sees it in its pass over the host.
func requestMachine(host *infrav1.PhysicalHost) *infrav1.Beskar7Machine {
	return &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
		Name: host.Spec.ConsumerRef.Name, Namespace: host.Namespace, UID: "request-machine-uid",
	}}
}

// requestMachineReconciler returns the Beskar7Machine controller with a BMC that
// answers and the given inspection timeout (zero for the default), reading and
// writing through the API server directly.
func requestMachineReconciler(inspectionTimeout time.Duration) *Beskar7MachineReconciler {
	return &Beskar7MachineReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(),
		Log:                  ctrl.Log.WithName("inspection-request-machine"),
		RedfishClientFactory: reachableBMC(),
		InspectionTimeout:    inspectionTimeout,
	}
}

// machinePass is one pass of the machine controller's state machine over the
// host as persisted, which is where every request is written from.
func machinePass(r *Beskar7MachineReconciler, key client.ObjectKey) (*infrav1.Beskar7Machine, error) {
	host := getPhysicalHost(key)
	machine := requestMachine(host)
	_, err := r.handlePhysicalHostState(ctx, r.Log, machine, host)
	return machine, err
}

// deliverInspect has the machine controller start the inspection of the InUse
// host it holds: it mints the credentials (or keeps the ones it has), boots the
// host and writes the signed inspect request.
func deliverInspect(key client.ObjectKey) {
	_, err := machinePass(requestMachineReconciler(0), key)
	Expect(err).NotTo(HaveOccurred())
}

// deliverInspectComplete has the machine controller validate the report of the
// Inspecting host it holds and write the signed inspect-complete request.
func deliverInspectComplete(key client.ObjectKey) {
	host := getPhysicalHost(key)
	host.Status.InspectionPhase = infrav1.InspectionPhaseComplete
	host.Status.InspectionReport = buildInspectionReport(InspectionReportRequest{
		Manufacturer: "Acme", Model: "Fast-1000", CPUs: []CPUData{{ID: "cpu0", Cores: 8}},
	})
	Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	ensureCallbackCredentials(key)

	machine, err := machinePass(requestMachineReconciler(0), key)
	Expect(err).NotTo(HaveOccurred())
	Expect(isTerminallyFailed(machine)).To(BeFalse())
}

// deliverInspectionTimeout has the machine controller time out the inspection
// of the Inspecting host it holds, which started a minute ago, and write the
// signed timeout request.
func deliverInspectionTimeout(key client.ObjectKey) {
	ensureCallbackCredentials(key)
	machine, err := machinePass(requestMachineReconciler(30*time.Second), key)
	Expect(err).NotTo(HaveOccurred())
	Expect(isTerminallyFailed(machine)).To(BeTrue())
}

// forgeInspectionRequest returns what someone allowed to patch the host would
// write: the request, with no binding. The host has its credentials, so what is
// missing is the binding.
func forgeInspectionRequest(value string) func(client.ObjectKey) {
	return func(key client.ObjectKey) {
		ensureCallbackCredentials(key)
		annotateHost(key, InspectionRequestAnnotation, value)
	}
}

// inspectionRequestValues are the values the machine controller writes, each
// with the state of a host it would change.
var inspectionRequestValues = []string{"inspect", "inspect-complete", "timeout"}

// frozenSecretClient returns a client that serves the host's bootstrap-token
// Secret from snapshot, as a cache that has not caught up with the Secret's
// last write does (nil: the Secret does not exist yet). Every other read, and
// every write, goes to the API server.
func frozenSecretClient(host client.ObjectKey, snapshot *corev1.Secret) client.Client {
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			secret, isSecret := obj.(*corev1.Secret)
			if !isSecret || key != (client.ObjectKey{Namespace: host.Namespace, Name: bootstrapTokenSecretName(host.Name)}) {
				return c.Get(ctx, key, obj, opts...)
			}
			if snapshot == nil {
				return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
			}
			snapshot.DeepCopyInto(secret)
			return nil
		},
	})
}

var _ = Describe("The Beskar7Machine controller's inspection requests are bound to the host's token (D-037)", func() {
	var (
		ns             *corev1.Namespace
		hostReconciler *PhysicalHostReconciler
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "inspection-request-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		hostReconciler = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("inspection-request-host"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: reachableBMC(),
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	expectRequestGone := func(h *infrav1.PhysicalHost) {
		Expect(h.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
		Expect(h.Annotations).NotTo(HaveKey(callbackBindingAnnotation(InspectionRequestAnnotation)))
	}

	Context("what the binding covers", func() {
		// Each request is delivered through the machine controller, then its
		// value is replaced with another request's and the binding left as the
		// machine wrote it. A binding that did not cover the value would let a
		// patcher turn a genuine inspect into a timeout.
		for _, delivered := range []struct {
			value   string
			deliver func(client.ObjectKey)
			state   string
		}{
			{"inspect", deliverInspect, infrav1.StateInUse},
			{"inspect-complete", deliverInspectComplete, infrav1.StateInspecting},
			{"timeout", deliverInspectionTimeout, infrav1.StateInspecting},
		} {
			delivered := delivered
			for _, attack := range inspectionRequestValues {
				if attack == delivered.value {
					continue
				}
				attack := attack
				It("ignores a genuine "+delivered.value+" whose value was changed to "+attack, func() {
					key := provisioningHost(ns.Name, "swap-host", "swap-machine", delivered.state, nil)
					delivered.deliver(key)
					Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, delivered.value))

					// An InUse host is changed by each of the three requests, so
					// whichever one the value is turned into would show.
					inUse := getPhysicalHost(key)
					inUse.Status.State = infrav1.StateInUse
					Expect(k8sClient.Status().Update(ctx, inUse)).To(Succeed())
					annotateHost(key, InspectionRequestAnnotation, attack)

					ignored := settlePhysicalHost(hostReconciler, key)
					Expect(ignored.Status.State).To(Equal(infrav1.StateInUse), "the changed value must not take effect")
					expectRequestGone(ignored)
				})
			}
		}

		It("ignores a request whose binding was computed for another annotation", func() {
			key := provisioningHost(ns.Name, "key-host", "key-machine", infrav1.StateInspecting, nil)
			ensureCallbackCredentials(key)
			binder := callbackBinderFor(key)
			annotateHost(key, InspectionRequestAnnotation, "timeout")
			annotateHost(key, callbackBindingAnnotation(InspectionRequestAnnotation),
				binder.mac(ProvisionedRequestAnnotation, "timeout", ""))

			ignored := settlePhysicalHost(hostReconciler, key)
			Expect(ignored.Status.State).To(Equal(infrav1.StateInspecting))
			expectRequestGone(ignored)
		})

		It("ignores a request bound to a report digest, which a request does not carry", func() {
			key := provisioningHost(ns.Name, "digest-host", "digest-machine", infrav1.StateInspecting, nil)
			ensureCallbackCredentials(key)
			binder := callbackBinderFor(key)
			annotateHost(key, InspectionRequestAnnotation, "timeout")
			annotateHost(key, callbackBindingAnnotation(InspectionRequestAnnotation),
				binder.mac(InspectionRequestAnnotation, "timeout", contentDigest("{}")))

			ignored := settlePhysicalHost(hostReconciler, key)
			Expect(ignored.Status.State).To(Equal(infrav1.StateInspecting))
			expectRequestGone(ignored)
		})

		It("is not replayed from an earlier claim, the machine's UID and the nonce having changed", func() {
			key := provisioningHost(ns.Name, "replay-host", "replay-machine", infrav1.StateInspecting, nil)
			deliverInspectionTimeout(key)
			captured := getPhysicalHost(key).Annotations

			By("the claim ending and the next one minting credentials of its own")
			releasePhysicalHost(key)
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateAvailable))
			setHostConsumer(key, "replay-next-machine")
			rewriteCredentials(key, func(data map[string][]byte) {
				data[bootstrapConsumerSecretKey] = []byte("replay-next-machine")
				data[bootstrapConsumerUIDSecretKey] = []byte("replay-next-uid")
				data[bootNonceSecretKey] = []byte("replay-next-nonce")
			})
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateInUse))

			By("a patcher replaying the earlier claim's request and its binding")
			annotateHost(key, InspectionRequestAnnotation, captured[InspectionRequestAnnotation])
			annotateHost(key, callbackBindingAnnotation(InspectionRequestAnnotation),
				captured[callbackBindingAnnotation(InspectionRequestAnnotation)])
			replayed := settlePhysicalHost(hostReconciler, key)
			Expect(replayed.Status.State).To(Equal(infrav1.StateInUse), "the earlier claim's timeout must not fail the next run")
			expectRequestGone(replayed)
		})
	})

	Context("how the machine controller signs", func() {
		It("signs inspect with the credentials its mint wrote, on the first claim and after a rotation", func() {
			key := provisioningHost(ns.Name, "mint-host", "mint-machine", infrav1.StateInUse, nil)

			By("the first claim: there is no Secret yet")
			deliverInspect(key)
			minted := getPhysicalHost(key)
			Expect(callbackBinderFor(key).holds(minted, InspectionRequestAnnotation, "")).To(BeTrue())
			firstBinder := callbackBinderFor(key)
			firstNonce := getCredentialSecret(key).Data[bootNonceSecretKey]

			By("the boot service fetching the nonce, so the next pass mints a fresh one")
			consumed := metav1.Now()
			booted := getPhysicalHost(key)
			booted.Status.Bootstrap = &infrav1.BootstrapStatus{
				BootNonceConsumedAt: &consumed, BootNonceConsumedHash: auth.Hash(string(firstNonce)),
			}
			Expect(k8sClient.Status().Update(ctx, booted)).To(Succeed())
			deliverInspect(key)

			rotated := getPhysicalHost(key)
			Expect(getCredentialSecret(key).Data[bootNonceSecretKey]).NotTo(Equal(firstNonce), "the nonce was rotated")
			Expect(callbackBinderFor(key).holds(rotated, InspectionRequestAnnotation, "")).To(BeTrue(),
				"the request is signed with the credentials the pass just wrote")
			Expect(firstBinder.holds(rotated, InspectionRequestAnnotation, "")).To(BeFalse(),
				"and not with the ones it replaced")
		})

		It("signs every request with the binding the reconciler recomputes, whatever the writer", func() {
			for _, name := range []string{"inspect request", "inspect-complete request", "timeout request"} {
				signal := callbackSignals[name]
				key := provisioningHost(ns.Name, "writer-"+strings.ReplaceAll(strings.Split(name, " ")[0], "-", ""), "writer-machine", signal.state, nil)
				signal.deliver(key)
				delivered := getPhysicalHost(key)
				Expect(delivered.Annotations).To(HaveKey(callbackBindingAnnotation(InspectionRequestAnnotation)), name)
				Expect(callbackBinderFor(key).holds(delivered, InspectionRequestAnnotation, "")).To(BeTrue(), name)
			}
		})

		It("writes the request and its binding in one patch", func() {
			key := provisioningHost(ns.Name, "patch-host", "patch-machine", infrav1.StateInspecting, nil)
			ensureCallbackCredentials(key)
			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			var (
				mu      sync.Mutex
				patches []string
			)
			recording := interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, isHost := obj.(*infrav1.PhysicalHost); isHost {
						data, err := patch.Data(obj)
						Expect(err).NotTo(HaveOccurred())
						mu.Lock()
						patches = append(patches, string(data))
						mu.Unlock()
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			r := requestMachineReconciler(0)
			r.Client = recording
			Expect(r.setInspectionRequestWithCurrentCredentials(ctx, r.Log, getPhysicalHost(key), "inspect-complete")).To(Succeed())

			mu.Lock()
			defer mu.Unlock()
			Expect(patches).To(HaveLen(1))
			Expect(patches[0]).To(ContainSubstring(InspectionRequestAnnotation))
			Expect(patches[0]).To(ContainSubstring(callbackBindingAnnotation(InspectionRequestAnnotation)))
		})

		DescribeTable("refuses to write a request it cannot sign",
			func(spoil func(client.ObjectKey)) {
				key := provisioningHost(ns.Name, "unsigned-host", "unsigned-machine", infrav1.StateInspecting, nil)
				spoil(key)
				host := getPhysicalHost(key)
				host.Status.InspectionPhase = infrav1.InspectionPhaseComplete
				host.Status.InspectionReport = buildInspectionReport(InspectionReportRequest{Manufacturer: "Acme"})
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

				By("the machine validating a report it cannot answer with a signed inspect-complete")
				_, err := machinePass(requestMachineReconciler(0), key)
				Expect(err).To(HaveOccurred())
				Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation),
					"an unsigned request would be removed unread; the machine reports the error instead")
				Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(callbackBindingAnnotation(InspectionRequestAnnotation)))

				By("the machine's inspection timeout running out, which still fails the machine")
				host = getPhysicalHost(key)
				host.Status.InspectionPhase = infrav1.InspectionPhaseBooting
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
				machine, err := machinePass(requestMachineReconciler(30*time.Second), key)
				Expect(err).NotTo(HaveOccurred())
				Expect(isTerminallyFailed(machine)).To(BeTrue())
				Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
			},
			Entry("a host with no credentials Secret", func(client.ObjectKey) {}),
			Entry("a Secret the host does not own", func(key client.ObjectKey) {
				ensureCallbackCredentials(key)
				data := getCredentialSecret(key).Data
				Expect(k8sClient.Delete(ctx, getCredentialSecret(key))).To(Succeed())
				Expect(k8sClient.Create(ctx, unownedCredentialSecret(key.Namespace, key.Name, data))).To(Succeed())
			}),
			Entry("a Secret minted for another machine", func(key client.ObjectKey) {
				ensureCallbackCredentials(key)
				rewriteCredentials(key, func(data map[string][]byte) { data[bootstrapConsumerSecretKey] = []byte("another-machine") })
			}),
			Entry("a Secret with no boot nonce", func(key client.ObjectKey) {
				ensureCallbackCredentials(key)
				rewriteCredentials(key, func(data map[string][]byte) { delete(data, bootNonceSecretKey) })
			}),
			Entry("a Secret with no bearer token", func(key client.ObjectKey) {
				ensureCallbackCredentials(key)
				rewriteCredentials(key, func(data map[string][]byte) { delete(data, bootstrapTokenSecretKey) })
			}),
		)

		It("signs a request after the token's expiry, which the verifier does not check either", func() {
			key := provisioningHost(ns.Name, "expired-host", "expired-machine", infrav1.StateInspecting, nil)
			ensureCallbackCredentials(key)
			rewriteCredentials(key, func(data map[string][]byte) {
				data[bootstrapTokenExpiresAtSecretKey] = credentialTime(time.Now().Add(-time.Minute))
			})
			deliverInspectionTimeout(key)
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "timeout"))

			applied := settlePhysicalHost(hostReconciler, key)
			Expect(applied.Status.State).To(Equal(infrav1.StateError))
			Expect(applied.Status.ErrorMessage).To(Equal(inspectionTimedOutMessage))
		})

		It("does not take a hand-set inspect for the request it wrote, and restarts the host it has not booted", func() {
			// A patcher setting inspect on an InUse host that is already on must
			// not make the machine believe the host is booting the inspector: the
			// reconciler removes the request unread, and the host would never boot.
			key := provisioningHost(ns.Name, "forged-pending-host", "forged-pending-machine", infrav1.StateInUse, nil)
			annotateHost(key, InspectionRequestAnnotation, "inspect")

			mockRf := internalredfish.NewMockClient()
			mockRf.PowerState = schemas.OnPowerState
			r := requestMachineReconciler(0)
			r.RedfishClientFactory = func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return mockRf, nil
			}
			_, err := machinePass(r, key)
			Expect(err).NotTo(HaveOccurred())

			Expect(mockRf.ResetCalled).To(BeTrue(), "an unsigned inspect is not a request the machine is waiting on")
			Expect(callbackBinderFor(key).holds(getPhysicalHost(key), InspectionRequestAnnotation, "")).To(BeTrue(),
				"the machine's own, signed request replaces it")
		})

		It("keeps a host it already booted from restarting when its own signed inspect is still pending", func() {
			key := provisioningHost(ns.Name, "pending-host", "pending-machine", infrav1.StateInUse, nil)
			mockRf := internalredfish.NewMockClient()
			mockRf.PowerState = schemas.OnPowerState
			newMachine := func() *Beskar7MachineReconciler {
				r := requestMachineReconciler(0)
				r.RedfishClientFactory = func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					return mockRf, nil
				}
				return r
			}
			_, err := machinePass(newMachine(), key)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockRf.ResetCalled).To(BeTrue())

			By("a restarted controller, which only has the request to go by")
			mockRf.ResetCalled = false
			_, err = machinePass(newMachine(), key)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockRf.ResetCalled).To(BeFalse(), "a second restart would interrupt the inspector booting")
		})
	})

	Context("how the reconciler reads the credentials", func() {
		It("applies an inspect written in the pass that minted the credentials, before its cache has seen them", func() {
			key := provisioningHost(ns.Name, "first-mint-host", "first-mint-machine", infrav1.StateInUse, nil)
			deliverInspect(key)

			By("a reconciler whose cache has no Secret yet")
			cached := &PhysicalHostReconciler{
				Client: frozenSecretClient(key, nil), APIReader: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("inspection-request-stale-host"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			applied := settlePhysicalHost(cached, key)
			Expect(applied.Status.State).To(Equal(infrav1.StateInspecting), "the host starts inspecting")
			expectRequestGone(applied)
		})

		It("applies an inspect signed under a rotated nonce, before its cache has seen the rotation", func() {
			key := provisioningHost(ns.Name, "rotated-host", "rotated-machine", infrav1.StateInUse, nil)
			deliverInspect(key)
			cachedSecret := getCredentialSecret(key)
			firstNonce := string(cachedSecret.Data[bootNonceSecretKey])

			By("the boot service fetching the nonce, so the next pass mints a fresh one, and the machine signing with it")
			consumed := metav1.Now()
			booted := getPhysicalHost(key)
			booted.Status.Bootstrap = &infrav1.BootstrapStatus{BootNonceConsumedAt: &consumed, BootNonceConsumedHash: auth.Hash(firstNonce)}
			Expect(k8sClient.Status().Update(ctx, booted)).To(Succeed())
			deliverInspect(key)
			Expect(string(getCredentialSecret(key).Data[bootNonceSecretKey])).NotTo(Equal(firstNonce))

			By("a reconciler whose cache still holds the Secret as the first mint wrote it")
			cached := &PhysicalHostReconciler{
				Client: frozenSecretClient(key, cachedSecret), APIReader: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("inspection-request-stale-host"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			applied := settlePhysicalHost(cached, key)
			Expect(applied.Status.State).To(Equal(infrav1.StateInspecting), "the genuine request is applied, not dropped")
			expectRequestGone(applied)
		})

		// The same holds for every signal the reconciler checks, not only the
		// request that was the reason to look: each is written by something that
		// has just read or written the Secret.
		for name, signal := range callbackSignals {
			name, signal := name, signal
			It("applies the "+name+" bound under a rotated nonce, before the reconciler's cache has seen the rotation", func() {
				key := provisioningHost(ns.Name, "stale-signal-host", "stale-signal-machine", signal.state, nil)
				ensureCallbackCredentials(key)
				cachedSecret := getCredentialSecret(key)
				rewriteCredentials(key, func(data map[string][]byte) { data[bootNonceSecretKey] = []byte("a-later-cycle-nonce") })
				signal.deliver(key)

				cached := &PhysicalHostReconciler{
					Client: frozenSecretClient(key, cachedSecret), APIReader: k8sClient, Scheme: k8sClient.Scheme(),
					Log:                  ctrl.Log.WithName("inspection-request-stale-signal-host"),
					Recorder:             record.NewFakeRecorder(10),
					RedfishClientFactory: reachableBMC(),
				}
				signal.applied(settlePhysicalHost(cached, key))
			})
		}

		It("signs inspect with the credentials its mint wrote, although its own cache cannot see the Secret", func() {
			// The machine reads the Secret through the same kind of cache: a second
			// read after the mint would find nothing, or the credentials before it.
			key := provisioningHost(ns.Name, "machine-cache-host", "machine-cache-machine", infrav1.StateInUse, nil)
			r := requestMachineReconciler(0)
			r.Client = frozenSecretClient(key, nil)

			_, err := machinePass(r, key)
			Expect(err).NotTo(HaveOccurred())

			Expect(callbackBinderFor(key).holds(getPhysicalHost(key), InspectionRequestAnnotation, "")).To(BeTrue(),
				"the request is signed with what the mint wrote")
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateInspecting))
		})

		It("would reject that same genuine request if it read the Secret through the stale cache", func() {
			// The control for the two specs above: the frozen client does hide the
			// credentials the request was signed with.
			key := provisioningHost(ns.Name, "control-host", "control-machine", infrav1.StateInUse, nil)
			deliverInspect(key)

			for name, stale := range map[string]client.Reader{
				"no Secret at all": frozenSecretClient(key, nil),
			} {
				host := getPhysicalHost(key)
				Expect(verifyCallbackAnnotation(ctx, stale, ctrl.Log, host, InspectionRequestAnnotation, "")).To(Equal(callbackRejected), name)
				Expect(host.Annotations).NotTo(HaveKey(InspectionRequestAnnotation), name)
			}
			fresh := getPhysicalHost(key)
			Expect(verifyCallbackAnnotation(ctx, k8sClient, ctrl.Log, fresh, InspectionRequestAnnotation, "")).To(Equal(callbackBound))
		})

		It("reads the Secret through APIReader, not through Client", func() {
			key := provisioningHost(ns.Name, "reader-host", "reader-machine", infrav1.StateInUse, nil)
			deliverInspect(key)

			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			countingSecretReads := func(reads *atomic.Int32) client.Client {
				return interceptor.NewClient(base, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, isSecret := obj.(*corev1.Secret); isSecret && k.Name == bootstrapTokenSecretName(key.Name) {
							reads.Add(1)
						}
						return c.Get(ctx, k, obj, opts...)
					},
				})
			}
			var viaClient, viaAPIReader atomic.Int32
			clientReads, apiReads := countingSecretReads(&viaClient), countingSecretReads(&viaAPIReader)
			reconciler := &PhysicalHostReconciler{
				Client: clientReads, APIReader: apiReads, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("inspection-request-reader-host"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			applied := settlePhysicalHost(reconciler, key)
			Expect(applied.Status.State).To(Equal(infrav1.StateInspecting))
			Expect(viaAPIReader.Load()).To(BeNumerically(">", 0), "the binding is checked against a live read")
			Expect(viaClient.Load()).To(BeNumerically(">", 0), "the Secret is still mirrored into status through Client")
		})

		It("leaves a request for the next pass when the Secret cannot be read live, and applies it once it can", func() {
			key := provisioningHost(ns.Name, "unreadable-request-host", "unreadable-request-machine", infrav1.StateInUse, nil)
			deliverInspect(key)

			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			var failing atomic.Bool
			failing.Store(true)
			flaky := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isSecret := obj.(*corev1.Secret); isSecret && k.Name == bootstrapTokenSecretName(key.Name) && failing.Load() {
						return errors.New("etcdserver: leader changed")
					}
					return c.Get(ctx, k, obj, opts...)
				},
			})
			reconciler := &PhysicalHostReconciler{
				Client: k8sClient, APIReader: flaky, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("inspection-request-unreadable"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			waiting := getPhysicalHost(key)
			Expect(waiting.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"),
				"a read error says nothing about the binding; dropping the request would lose a real one to an API blip")
			Expect(waiting.Annotations).To(HaveKey(callbackBindingAnnotation(InspectionRequestAnnotation)))
			Expect(waiting.Status.State).To(Equal(infrav1.StateInUse))

			failing.Store(false)
			Expect(settlePhysicalHost(reconciler, key).Status.State).To(Equal(infrav1.StateInspecting))
		})

		It("is wired to the manager's API reader by SetupWithManager, and keeps one it was given", func() {
			skipNameValidation := true
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:                 k8sClient.Scheme(),
				Metrics:                metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0",
				Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{ns.Name: {}}},
				Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
			})
			Expect(err).NotTo(HaveOccurred())

			unset := &PhysicalHostReconciler{
				Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				Log:                  ctrl.Log.WithName("inspection-request-setup"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			Expect(unset.SetupWithManager(mgr)).To(Succeed())
			Expect(unset.APIReader).To(BeIdenticalTo(mgr.GetAPIReader()),
				"a manager-wired reconciler checks bindings against the API server, not the cache it reads hosts from")
			Expect(unset.bindingReader()).To(BeIdenticalTo(mgr.GetAPIReader()))

			given := &PhysicalHostReconciler{
				Client: mgr.GetClient(), APIReader: k8sClient, Scheme: mgr.GetScheme(),
				Log:                  ctrl.Log.WithName("inspection-request-setup"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			Expect(given.SetupWithManager(mgr)).To(Succeed())
			Expect(given.APIReader).To(BeIdenticalTo(k8sClient))
		})

		It("starts the inspection through a running manager, whose caches lag its writes", func() {
			key := provisioningHost(ns.Name, "managed-host", "managed-machine", infrav1.StateInUse, nil)

			skipNameValidation := true
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:                 k8sClient.Scheme(),
				Metrics:                metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0",
				// envtest never finishes deleting namespaces, so a cluster-wide cache
				// would hand this controller every other spec's leftovers.
				Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{ns.Name: {}}},
				Controller: config.Controller{SkipNameValidation: &skipNameValidation},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect((&PhysicalHostReconciler{
				Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				Log:                  ctrl.Log.WithName("inspection-request-managed-host"),
				Recorder:             record.NewFakeRecorder(100),
				RedfishClientFactory: reachableBMC(),
			}).SetupWithManager(mgr)).To(Succeed())
			mgrCtx, mgrCancel := context.WithCancel(ctx)
			defer mgrCancel()
			go func() {
				defer GinkgoRecover()
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()
			Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

			By("the machine controller, reading and writing through the manager's client, starting the inspection")
			machineR := requestMachineReconciler(0)
			machineR.Client = mgr.GetClient()
			host := &infrav1.PhysicalHost{}
			Expect(mgr.GetClient().Get(ctx, key, host)).To(Succeed())
			_, err = machineR.triggerInspection(ctx, machineR.Log, requestMachine(host), host)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := getPhysicalHost(key)
				g.Expect(got.Status.State).To(Equal(infrav1.StateInspecting))
				g.Expect(got.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
			}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
		})
	})

	Context("a request when the claim has ended", func() {
		It("is removed unread, and the host goes back to Available", func() {
			key := provisioningHost(ns.Name, "released-host", "released-machine", infrav1.StateInUse, nil)
			deliverInspect(key)
			releasePhysicalHost(key)

			released := settlePhysicalHost(hostReconciler, key)
			Expect(released.Status.State).To(Equal(infrav1.StateAvailable))
			expectRequestGone(released)
		})
	})

	Context("an upgrade from a release that wrote the requests unsigned", func() {
		// The old controller's request that was still waiting when the new one
		// started has no binding, so it is removed. The machine controller keeps
		// writing the request for as long as the host has not moved on, so it
		// writes it again, signed, in its next pass.
		It("restarts an inspect that was waiting: the machine writes it again, signed, and the host starts inspecting", func() {
			key := provisioningHost(ns.Name, "upgrade-inspect-host", "upgrade-inspect-machine", infrav1.StateInUse, nil)
			ensureCallbackCredentials(key)
			annotateHost(key, InspectionRequestAnnotation, "inspect")

			By("the upgraded reconciler meeting the old controller's request")
			dropped := settlePhysicalHost(hostReconciler, key)
			Expect(dropped.Status.State).To(Equal(infrav1.StateInUse), "the unsigned request is not acted on")
			expectRequestGone(dropped)

			By("the machine's next pass over the host, which is still InUse")
			_, err := machinePass(requestMachineReconciler(0), key)
			Expect(err).NotTo(HaveOccurred())
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))

			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateInspecting))
		})

		It("restarts an inspect-complete that was waiting: the machine writes it again, signed, and the host starts deploying", func() {
			key := provisioningHost(ns.Name, "upgrade-complete-host", "upgrade-complete-machine", infrav1.StateInspecting, nil)
			host := getPhysicalHost(key)
			host.Status.InspectionPhase = infrav1.InspectionPhaseComplete
			host.Status.InspectionReport = buildInspectionReport(InspectionReportRequest{
				Manufacturer: "Acme", Model: "Fast-1000", CPUs: []CPUData{{ID: "cpu0", Cores: 8}},
			})
			setTrue(host, infrav1.HostInspectedCondition, infrav1.HostInspectedReason)
			Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			ensureCallbackCredentials(key)
			annotateHost(key, InspectionRequestAnnotation, "inspect-complete")

			By("the upgraded reconciler meeting the old controller's request")
			dropped := settlePhysicalHost(hostReconciler, key)
			Expect(dropped.Status.State).To(Equal(infrav1.StateInspecting))
			Expect(dropped.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete), "the report the machine validates is still there")
			expectRequestGone(dropped)

			By("the machine's next pass over the host, which still has a report to validate")
			machine, err := machinePass(requestMachineReconciler(0), key)
			Expect(err).NotTo(HaveOccurred())
			Expect(isTerminallyFailed(machine)).To(BeFalse())
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"))

			deploying := settlePhysicalHost(hostReconciler, key)
			Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying))
			Expect(deploying.Status.DeployingTimestamp).NotTo(BeNil())
		})

		It("leaves a timeout that was waiting removed, the machine having failed for it already", func() {
			key := provisioningHost(ns.Name, "upgrade-timeout-host", "upgrade-timeout-machine", infrav1.StateInspecting, nil)
			ensureCallbackCredentials(key)
			annotateHost(key, InspectionRequestAnnotation, "timeout")

			dropped := settlePhysicalHost(hostReconciler, key)
			Expect(dropped.Status.State).To(Equal(infrav1.StateInspecting))
			expectRequestGone(dropped)
		})
	})

	Context("the reconciler's log", func() {
		It("names the host and the annotation for a request that is not bound, and nothing the request carried", func() {
			key := provisioningHost(ns.Name, "logged-request-host", "logged-request-machine", infrav1.StateInspecting, nil)
			token := ensureCallbackCredentials(key)
			annotateHost(key, InspectionRequestAnnotation, "timeout")
			binding := strings.Repeat("d", 64)
			annotateHost(key, callbackBindingAnnotation(InspectionRequestAnnotation), binding)

			var mu sync.Mutex
			var logged strings.Builder
			capture := funcr.New(func(prefix, args string) {
				mu.Lock()
				defer mu.Unlock()
				logged.WriteString(prefix + " " + args + "\n")
			}, funcr.Options{Verbosity: 4})
			reconciler := &PhysicalHostReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  logr.New(capture.GetSink()),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			mu.Lock()
			defer mu.Unlock()
			var rejection string
			for _, line := range strings.Split(logged.String(), "\n") {
				if strings.Contains(line, "not bound to the host's credentials") {
					rejection = line
				}
			}
			Expect(rejection).To(ContainSubstring("logged-request-host"))
			Expect(rejection).To(ContainSubstring(InspectionRequestAnnotation))
			Expect(logged.String()).NotTo(ContainSubstring(token), "the bearer token is never logged")
			Expect(logged.String()).NotTo(ContainSubstring(binding), "a binding is never logged")
			Expect(logged.String()).NotTo(ContainSubstring("Applying inspection-request"), "an unbound request is not acted on")
			Expect(logged.String()).NotTo(ContainSubstring("Recording inspection timeout"))
			Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateInspecting))
		})

		It("does not log the value of an unbound request that is not one of the three either", func() {
			key := provisioningHost(ns.Name, "odd-request-host", "odd-request-machine", infrav1.StateInspecting, nil)
			ensureCallbackCredentials(key)
			annotateHost(key, InspectionRequestAnnotation, "value-that-must-not-be-logged")

			var mu sync.Mutex
			var logged strings.Builder
			capture := funcr.New(func(prefix, args string) {
				mu.Lock()
				defer mu.Unlock()
				logged.WriteString(prefix + " " + args + "\n")
			}, funcr.Options{Verbosity: 4})
			reconciler := &PhysicalHostReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  logr.New(capture.GetSink()),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			mu.Lock()
			defer mu.Unlock()
			Expect(logged.String()).NotTo(ContainSubstring("value-that-must-not-be-logged"))
			expectRequestGone(getPhysicalHost(key))
		})
	})

	Context("a request that is bound", func() {
		It("is removed with its binding, and keeps the existing behaviour of the run it is for", func() {
			By("a failed run ignoring a request, as before")
			failedKey := provisioningHost(ns.Name, "failed-run-host", "failed-run-machine", infrav1.StateError, nil)
			failed := getPhysicalHost(failedKey)
			failed.Status.ErrorMessage = provisionFailedReasonPrefix + "disk write failed"
			Expect(k8sClient.Status().Update(ctx, failed)).To(Succeed())
			requestInspection(failedKey, "inspect-complete")
			afterFailure := settlePhysicalHost(hostReconciler, failedKey)
			Expect(afterFailure.Status.State).To(Equal(infrav1.StateError), "a failed run's Error ends only at release")
			expectRequestGone(afterFailure)

			By("a provisioned host ignoring a request, as before")
			readyKey := provisioningHost(ns.Name, "ready-run-host", "ready-run-machine", infrav1.StateReady, nil)
			requestInspection(readyKey, "timeout")
			afterReady := settlePhysicalHost(hostReconciler, readyKey)
			Expect(afterReady.Status.State).To(Equal(infrav1.StateReady))
			expectRequestGone(afterReady)
		})

		It("is removed with its binding in the pass that consumes it, whichever way the pass ends", func() {
			// One reconcile, not a settled host: a binding left behind would be
			// removed by the next pass as an orphan, and the spec would not see it.
			onePass := func(name string, state string, stage func(client.ObjectKey)) {
				key := provisioningHost(ns.Name, name, name+"-machine", state, nil)
				stage(key)
				requestInspection(key, "inspect")
				Expect(getPhysicalHost(key).Annotations).To(HaveKey(callbackBindingAnnotation(InspectionRequestAnnotation)))

				_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				expectRequestGone(getPhysicalHost(key))
			}
			onePass("one-pass-applied", infrav1.StateInUse, func(client.ObjectKey) {})
			onePass("one-pass-failed", infrav1.StateError, func(key client.ObjectKey) {
				failed := getPhysicalHost(key)
				failed.Status.ErrorMessage = provisionFailedReasonPrefix + "disk write failed"
				Expect(k8sClient.Status().Update(ctx, failed)).To(Succeed())
			})
			onePass("one-pass-ready", infrav1.StateReady, func(client.ObjectKey) {})
		})

		It("is an orphan binding's to remove, when the request next to it is gone", func() {
			key := provisioningHost(ns.Name, "orphan-host", "orphan-machine", infrav1.StateInspecting, nil)
			annotateHost(key, callbackBindingAnnotation(InspectionRequestAnnotation), strings.Repeat("a", 64))
			annotateHost(key, "example.com/unrelated", "kept")
			ignored := settlePhysicalHost(hostReconciler, key)
			expectRequestGone(ignored)
			Expect(ignored.Annotations).To(HaveKeyWithValue("example.com/unrelated", "kept"))
		})
	})
})

var _ = Describe("newRequestSigner", func() {
	It("signs what the verifier checks, and refuses what it would refuse", func() {
		host := &infrav1.PhysicalHost{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "host", UID: "host-uid"}}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: bootstrapTokenSecretName(host.Name), Namespace: host.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: infrav1.GroupVersion.String(), Kind: "PhysicalHost", Name: host.Name, UID: host.UID,
					Controller: ptr.To(true),
				}},
			},
			Data: boundCredentialData("machine", "token", time.Hour, "nonce", time.Minute),
		}
		secret.Data[bootstrapConsumerUIDSecretKey] = []byte("machine-uid")
		reader := fakeSecretReader{secret}
		host.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion, Name: "machine", Namespace: host.Namespace,
		}

		signer, err := newRequestSigner(ctx, reader, host)
		Expect(err).NotTo(HaveOccurred())
		signed := host.DeepCopy()
		signer.setAnnotation(signed, InspectionRequestAnnotation, "timeout", "")

		verifier, err := newCallbackBinder(host, bootstrapCredentials{token: "token", nonce: "nonce", consumerUID: "machine-uid"})
		Expect(err).NotTo(HaveOccurred())
		Expect(verifier.holds(signed, InspectionRequestAnnotation, "")).To(BeTrue(), "the verifier recomputes the signer's binding")

		unclaimed := host.DeepCopy()
		unclaimed.Spec.ConsumerRef = nil
		_, err = newRequestSigner(ctx, reader, unclaimed)
		Expect(err).To(HaveOccurred(), "a host nobody claims has no credentials to sign with")

		foreign := host.DeepCopy()
		foreign.UID = "another-uid"
		_, err = newRequestSigner(ctx, reader, foreign)
		Expect(err).To(MatchError(errBootstrapSecretNotOwned))

		another := host.DeepCopy()
		another.Spec.ConsumerRef.Name = "another-machine"
		_, err = newRequestSigner(ctx, reader, another)
		Expect(err).To(HaveOccurred(), "credentials minted for another machine do not sign for this claim")

		noNonce := secret.DeepCopy()
		delete(noNonce.Data, bootNonceSecretKey)
		_, err = newRequestSigner(ctx, fakeSecretReader{noNonce}, host)
		Expect(err).To(MatchError(errCallbackBindingUnavailable))
	})
})

// fakeSecretReader serves one Secret, whatever is asked for.
type fakeSecretReader struct{ secret *corev1.Secret }

func (f fakeSecretReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	f.secret.DeepCopyInto(obj.(*corev1.Secret))
	return nil
}

func (f fakeSecretReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("not implemented")
}
