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
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// Three PhysicalHost annotations drive the host's status, and only the callback
// handlers are meant to write them: the inspection report's reference, the
// /provisioned report and the /provision-failed report. Anyone allowed to patch
// PhysicalHosts could write them too, and push a host to Ready, inject a
// hardware report or fail a run without an inspector (SEC-15). Each now carries
// a binding, an HMAC keyed by the host's bearer token, and the reconciler acts
// only on a signal whose binding it can recompute from the host's
// bootstrap-token Secret (D-034). These specs pin that, per signal, against the
// real handlers and the real PhysicalHostReconciler.

var _ = Describe("callbackBinder", func() {
	newHost := func() *infrav1.PhysicalHost {
		return &infrav1.PhysicalHost{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "host", UID: "host-uid"}}
	}
	creds := bootstrapCredentials{token: "token", nonce: "nonce", consumerUID: "consumer-uid"}
	binderFor := func(host *infrav1.PhysicalHost, c bootstrapCredentials) *callbackBinder {
		b, err := newCallbackBinder(host, c)
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	const key, value, digest = ProvisionedRequestAnnotation, "provisioned", ""
	base := binderFor(newHost(), creds).mac(key, value, digest)

	It("is the same for the same inputs, and hex SHA-256 sized", func() {
		Expect(binderFor(newHost(), creds).mac(key, value, digest)).To(Equal(base))
		Expect(base).To(MatchRegexp(`^[0-9a-f]{64}$`))
	})

	It("changes with every field it is meant to bind", func() {
		changed := func(mutate func(*infrav1.PhysicalHost, *bootstrapCredentials) (string, string, string)) string {
			host, c := newHost(), creds
			k, v, d := mutate(host, &c)
			return binderFor(host, c).mac(k, v, d)
		}
		for name, mutate := range map[string]func(*infrav1.PhysicalHost, *bootstrapCredentials) (string, string, string){
			"the annotation key": func(*infrav1.PhysicalHost, *bootstrapCredentials) (string, string, string) {
				return ProvisionFailedRequestAnnotation, value, digest
			},
			"the value": func(*infrav1.PhysicalHost, *bootstrapCredentials) (string, string, string) {
				return key, "provisioned2", digest
			},
			"the content digest": func(*infrav1.PhysicalHost, *bootstrapCredentials) (string, string, string) {
				return key, value, contentDigest("x")
			},
			"the host namespace": func(h *infrav1.PhysicalHost, _ *bootstrapCredentials) (string, string, string) {
				h.Namespace = "other"
				return key, value, digest
			},
			"the host name": func(h *infrav1.PhysicalHost, _ *bootstrapCredentials) (string, string, string) {
				h.Name = "other"
				return key, value, digest
			},
			"the host UID": func(h *infrav1.PhysicalHost, _ *bootstrapCredentials) (string, string, string) {
				h.UID = "other"
				return key, value, digest
			},
			"the consumer UID": func(_ *infrav1.PhysicalHost, c *bootstrapCredentials) (string, string, string) {
				c.consumerUID = "other"
				return key, value, digest
			},
			"the boot nonce": func(_ *infrav1.PhysicalHost, c *bootstrapCredentials) (string, string, string) {
				c.nonce = "other"
				return key, value, digest
			},
			"the bearer token": func(_ *infrav1.PhysicalHost, c *bootstrapCredentials) (string, string, string) {
				c.token = "other"
				return key, value, digest
			},
		} {
			Expect(changed(mutate)).NotTo(Equal(base), "the binding does not cover %s", name)
		}
	})

	It("does not let one field's tail run into the next", func() {
		// The value is last, and every field before it is a name, a UID or a hex
		// digest: moving a character across a boundary must not keep the MAC.
		a := binderFor(newHost(), bootstrapCredentials{token: "t", nonce: "n", consumerUID: "ab"}).mac(key, "c", "")
		b := binderFor(newHost(), bootstrapCredentials{token: "t", nonce: "n", consumerUID: "a"}).mac(key, "bc", "")
		Expect(a).NotTo(Equal(b))
	})

	It("refuses credentials that cannot bind a signal", func() {
		for name, c := range map[string]bootstrapCredentials{
			"no token (an empty key would let anyone compute a binding)": {nonce: "nonce", consumerUID: "uid"},
			"no boot nonce (nothing tells one boot cycle from the next)": {token: "token", consumerUID: "uid"},
		} {
			_, err := newCallbackBinder(newHost(), c)
			Expect(err).To(MatchError(errCallbackBindingUnavailable), name)
		}
	})

	It("accepts a Secret that records no consumer UID, which the token and nonce still tie to one cycle", func() {
		_, err := newCallbackBinder(newHost(), bootstrapCredentials{token: "token", nonce: "nonce"})
		Expect(err).NotTo(HaveOccurred())
	})

	It("holds only for the signal and binding it was computed for", func() {
		host := newHost()
		binder := binderFor(host, creds)
		binder.setAnnotation(host, key, value, digest)
		Expect(binder.holds(host, key, digest)).To(BeTrue())

		host.Annotations[callbackBindingAnnotation(key)] = strings.ToUpper(host.Annotations[callbackBindingAnnotation(key)])
		Expect(binder.holds(host, key, digest)).To(BeFalse(), "the comparison is of the exact text the handler wrote")
		delete(host.Annotations, callbackBindingAnnotation(key))
		Expect(binder.holds(host, key, digest)).To(BeFalse())
	})
})

// callbackSignal is one of the three annotations, with what it takes to
// deliver it through its handler, forge it, and tell whether it took effect.
type callbackSignal struct {
	annotation string
	// state the host is in when the signal arrives.
	state string
	// deliver posts the signal the way the inspector does, through the handler.
	deliver func(key client.ObjectKey)
	// forge writes the same signal as someone allowed to patch the host would:
	// the value, and no binding.
	forge func(key client.ObjectKey)
	// digest is the content digest the signal's binding covers.
	digest func(key client.ObjectKey) string
	// applied and untouched say what the host looks like after a signal took
	// effect and after one did not.
	applied   func(*infrav1.PhysicalHost)
	untouched func(*infrav1.PhysicalHost)
}

var callbackSignals = map[string]callbackSignal{
	"inspection report": {
		annotation: InspectionResultAnnotation,
		state:      infrav1.StateInspecting,
		deliver:    postInspectionReport,
		forge: func(key client.ObjectKey) {
			forged, err := json.Marshal(&infrav1.InspectionReport{Manufacturer: "Forged", Model: "Forged-1"})
			Expect(err).NotTo(HaveOccurred())
			name := inspectionResultConfigMapName(key.Name)
			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace},
				Data:       map[string]string{inspectionResultDataKey: string(forged)},
			})).To(Succeed())
			annotateHost(key, InspectionResultAnnotation, name)
		},
		digest: func(key client.ObjectKey) string {
			return contentDigest(resultConfigMapReport(key.Namespace, inspectionResultConfigMapName(key.Name)))
		},
		applied: func(h *infrav1.PhysicalHost) {
			Expect(h.Status.InspectionReport).NotTo(BeNil())
			Expect(h.Status.InspectionReport.Manufacturer).To(Equal("Acme"))
			Expect(h.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete))
			Expect(conditions.IsTrue(h, infrav1.HostInspectedCondition)).To(BeTrue())
		},
		untouched: func(h *infrav1.PhysicalHost) {
			Expect(h.Status.InspectionReport).To(BeNil(), "an injected report must not reach the status")
			Expect(h.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseBooting))
			Expect(conditions.IsTrue(h, infrav1.HostInspectedCondition)).To(BeFalse())
		},
	},
	"/provisioned report": {
		annotation: ProvisionedRequestAnnotation,
		state:      infrav1.StateDeploying,
		deliver:    reportProvisioned,
		forge:      func(key client.ObjectKey) { annotateHost(key, ProvisionedRequestAnnotation, "provisioned") },
		digest:     func(client.ObjectKey) string { return "" },
		applied: func(h *infrav1.PhysicalHost) {
			Expect(h.Status.State).To(Equal(infrav1.StateReady))
		},
		untouched: func(h *infrav1.PhysicalHost) {
			Expect(h.Status.State).To(Equal(infrav1.StateDeploying), "a host must not reach Ready without an inspector")
		},
	},
	"/provision-failed report": {
		annotation: ProvisionFailedRequestAnnotation,
		state:      infrav1.StateDeploying,
		deliver:    func(key client.ObjectKey) { reportDeployFailure(key, sanitizeFailureReason("disk write failed")) },
		forge: func(key client.ObjectKey) {
			annotateHost(key, ProvisionFailedRequestAnnotation, sanitizeFailureReason("forged failure"))
		},
		digest: func(client.ObjectKey) string { return "" },
		applied: func(h *infrav1.PhysicalHost) {
			Expect(h.Status.State).To(Equal(infrav1.StateError))
			Expect(h.Status.ErrorMessage).To(Equal(sanitizeFailureReason("disk write failed")))
		},
		untouched: func(h *infrav1.PhysicalHost) {
			Expect(h.Status.State).To(Equal(infrav1.StateDeploying), "a run must not fail without an inspector")
			Expect(h.Status.ErrorMessage).To(BeEmpty())
		},
	},
}

// callbackTamper is something done to a delivered signal, or to what its
// binding was computed from, after the handler wrote it.
type callbackTamper func(key client.ObjectKey, signal callbackSignal)

var callbackTampers = map[string]callbackTamper{
	"its binding removed": func(key client.ObjectKey, signal callbackSignal) {
		removeHostAnnotations(key, callbackBindingAnnotation(signal.annotation))
	},
	"its binding replaced with text that is not its MAC": func(key client.ObjectKey, signal callbackSignal) {
		annotateHost(key, callbackBindingAnnotation(signal.annotation), strings.Repeat("0", 64))
	},
	"its binding replaced with one computed under another host's token": func(key client.ObjectKey, signal callbackSignal) {
		other := provisioningHost(key.Namespace, "other-"+key.Name, "other-machine", infrav1.StateDeploying, nil)
		ensureCallbackCredentials(other)
		value := getPhysicalHost(key).Annotations[signal.annotation]
		annotateHost(key, callbackBindingAnnotation(signal.annotation),
			callbackBinderFor(other).mac(signal.annotation, value, signal.digest(key)))
	},
	"its value edited": func(key client.ObjectKey, signal callbackSignal) {
		annotateHost(key, signal.annotation, getPhysicalHost(key).Annotations[signal.annotation]+"-edited")
	},
	"the boot nonce rotated, as a later boot cycle of the claim does": func(key client.ObjectKey, _ callbackSignal) {
		rewriteCredentials(key, func(data map[string][]byte) { data[bootNonceSecretKey] = []byte("a-later-cycle-nonce") })
	},
	"the consumer's UID changed": func(key client.ObjectKey, _ callbackSignal) {
		rewriteCredentials(key, func(data map[string][]byte) { data[bootstrapConsumerUIDSecretKey] = []byte("another-uid") })
	},
	"the bearer token replaced": func(key client.ObjectKey, _ callbackSignal) {
		rewriteCredentials(key, func(data map[string][]byte) { data[bootstrapTokenSecretKey] = []byte("a-replacement-token") })
	},
	"the host claimed by another machine": func(key client.ObjectKey, _ callbackSignal) {
		setHostConsumer(key, "another-machine")
	},
	"the credentials Secret deleted": func(key client.ObjectKey, _ callbackSignal) {
		Expect(k8sClient.Delete(ctx, getCredentialSecret(key))).To(Succeed())
	},
	"the credentials Secret replaced by one the host does not own": func(key client.ObjectKey, _ callbackSignal) {
		data := getCredentialSecret(key).Data
		Expect(k8sClient.Delete(ctx, getCredentialSecret(key))).To(Succeed())
		Expect(k8sClient.Create(ctx, unownedCredentialSecret(key.Namespace, key.Name, data))).To(Succeed())
	},
}

// rewriteCredentials changes the data of the host's bootstrap-token Secret.
func rewriteCredentials(host client.ObjectKey, edit func(map[string][]byte)) {
	secret := getCredentialSecret(host)
	edit(secret.Data)
	Expect(k8sClient.Update(ctx, secret)).To(Succeed())
}

// removeHostAnnotations removes exactly keys from the host.
func removeHostAnnotations(host client.ObjectKey, keys ...string) {
	Expect(k8sClient.Patch(ctx, getPhysicalHost(host), removeAnnotationsPatch(keys...))).To(Succeed())
}

var _ = Describe("Callback signals are bound to the host's per-host token (SEC-15, D-034)", func() {
	var (
		ns             *corev1.Namespace
		hostReconciler *PhysicalHostReconciler
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "callback-binding-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		hostReconciler = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("callback-binding-host"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: reachableBMC(),
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	signalNames := make([]string, 0, len(callbackSignals))
	for name := range callbackSignals {
		signalNames = append(signalNames, name)
	}

	for _, name := range signalNames {
		name := name
		Context("the "+name, func() {
			var signal callbackSignal

			BeforeEach(func() { signal = callbackSignals[name] })

			stage := func() client.ObjectKey {
				return provisioningHost(ns.Name, "bound-host", "bound-machine", signal.state, nil)
			}

			expectGone := func(h *infrav1.PhysicalHost) {
				Expect(h.Annotations).NotTo(HaveKey(signal.annotation))
				Expect(h.Annotations).NotTo(HaveKey(callbackBindingAnnotation(signal.annotation)))
			}

			It("is applied when the handler wrote it, and its binding goes with it", func() {
				key := stage()
				signal.deliver(key)
				delivered := getPhysicalHost(key)
				Expect(delivered.Annotations).To(HaveKey(signal.annotation))
				Expect(delivered.Annotations).To(HaveKey(callbackBindingAnnotation(signal.annotation)),
					"the handler writes the binding next to the signal")

				applied := settlePhysicalHost(hostReconciler, key)
				signal.applied(applied)
				expectGone(applied)
			})

			It("is ignored and removed when someone patched it in without a binding", func() {
				key := stage()
				signal.forge(key)

				ignored := settlePhysicalHost(hostReconciler, key)
				signal.untouched(ignored)
				expectGone(ignored)
			})

			It("is removed, and nothing else is, when a binding is left with no signal", func() {
				key := stage()
				annotateHost(key, callbackBindingAnnotation(signal.annotation), strings.Repeat("a", 64))
				annotateHost(key, "example.com/unrelated", "kept")

				ignored := settlePhysicalHost(hostReconciler, key)
				expectGone(ignored)
				Expect(ignored.Annotations).To(HaveKeyWithValue("example.com/unrelated", "kept"),
					"the removal is by key; other annotations stay")
				signal.untouched(ignored)
			})

			It("is removed by key, leaving the host's other annotations", func() {
				key := stage()
				annotateHost(key, "example.com/unrelated", "kept")
				signal.forge(key)

				ignored := settlePhysicalHost(hostReconciler, key)
				signal.untouched(ignored)
				expectGone(ignored)
				Expect(ignored.Annotations).To(HaveKeyWithValue("example.com/unrelated", "kept"))
			})

			for tamperName, tamper := range callbackTampers {
				tamperName, tamper := tamperName, tamper
				It("is ignored and removed, with the state unchanged, with "+tamperName, func() {
					key := stage()
					signal.deliver(key)
					tamper(key, signal)

					ignored := settlePhysicalHost(hostReconciler, key)
					signal.untouched(ignored)
					expectGone(ignored)
				})
			}
		})
	}

	Context("the inspection report's ConfigMap", func() {
		It("is not deleted when the annotation naming it is not bound", func() {
			key := provisioningHost(ns.Name, "cm-host", "cm-machine", infrav1.StateInspecting, nil)
			bystander := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "somebody-elses-config", Namespace: ns.Name},
				Data:       map[string]string{inspectionResultDataKey: `{"manufacturer":"Acme"}`},
			}
			Expect(k8sClient.Create(ctx, bystander)).To(Succeed())
			annotateHost(key, InspectionResultAnnotation, bystander.Name)

			ignored := settlePhysicalHost(hostReconciler, key)
			Expect(ignored.Annotations).NotTo(HaveKey(InspectionResultAnnotation))
			Expect(ignored.Status.InspectionReport).To(BeNil())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(bystander), &corev1.ConfigMap{})).To(Succeed(),
				"the annotation chooses the name; consuming the report must not be a way to delete a ConfigMap")
		})

		It("is not deleted when it is any other ConfigMap, whatever it holds", func() {
			key := provisioningHost(ns.Name, "other-cm-host", "other-cm-machine", infrav1.StateInspecting, nil)
			for name, data := range map[string]map[string]string{
				"holds-no-report":      {"unrelated": "value"},
				"holds-broken-json":    {inspectionResultDataKey: "{not json"},
				"holds-a-valid-report": {inspectionResultDataKey: `{"manufacturer":"Acme"}`},
			} {
				bystander := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name}, Data: data}
				Expect(k8sClient.Create(ctx, bystander)).To(Succeed())
				annotateHost(key, InspectionResultAnnotation, name)

				ignored := settlePhysicalHost(hostReconciler, key)
				Expect(ignored.Annotations).NotTo(HaveKey(InspectionResultAnnotation), name)
				Expect(ignored.Status.InspectionReport).To(BeNil(), name)
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(bystander), &corev1.ConfigMap{})).To(Succeed(),
					"a ConfigMap that %s must survive an annotation that names it without a binding", name)
			}
		})

		It("is ignored when its content changed after the handler bound it", func() {
			key := provisioningHost(ns.Name, "rewritten-host", "rewritten-machine", infrav1.StateInspecting, nil)
			postInspectionReport(key)

			cm := &corev1.ConfigMap{}
			cmKey := client.ObjectKey{Namespace: key.Namespace, Name: inspectionResultConfigMapName(key.Name)}
			Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
			forged, err := json.Marshal(&infrav1.InspectionReport{Manufacturer: "Forged", Model: "Forged-1"})
			Expect(err).NotTo(HaveOccurred())
			cm.Data[inspectionResultDataKey] = string(forged)
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			ignored := settlePhysicalHost(hostReconciler, key)
			Expect(ignored.Status.InspectionReport).To(BeNil(), "the report the handler bound is not the one the ConfigMap holds now")
			Expect(ignored.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseBooting))
			Expect(ignored.Annotations).NotTo(HaveKey(InspectionResultAnnotation))
			Expect(ignored.Annotations).NotTo(HaveKey(callbackBindingAnnotation(InspectionResultAnnotation)))
			Expect(k8sClient.Get(ctx, cmKey, &corev1.ConfigMap{})).To(Succeed(), "an unbound report's ConfigMap is left alone")
		})

		It("is bound to the report the handler stored when a second report replaces the first", func() {
			key := provisioningHost(ns.Name, "second-host", "second-machine", infrav1.StateInspecting, nil)
			token := ensureCallbackCredentials(key)
			handler := &InspectionHandler{Client: k8sClient, Log: ctrl.Log.WithName("callback-binding-second")}
			Expect(handler.processInspectionReport(ctx, handler.Log, key.Namespace, key.Name, token,
				InspectionReportRequest{Manufacturer: "Acme", Model: "First"})).To(Succeed())
			Expect(handler.processInspectionReport(ctx, handler.Log, key.Namespace, key.Name, token,
				InspectionReportRequest{Manufacturer: "Acme", Model: "Second"})).To(Succeed())

			applied := settlePhysicalHost(hostReconciler, key)
			Expect(applied.Status.InspectionReport).NotTo(BeNil())
			Expect(applied.Status.InspectionReport.Model).To(Equal("Second"), "the binding follows the stored report")
		})
	})

	Context("a hand-set inspection-request", func() {
		It("takes a host to Deploying with inspect-complete, and no further without a bound /provisioned", func() {
			key := provisioningHost(ns.Name, "complete-host", "complete-machine", infrav1.StateInspecting, nil)
			ensureCallbackCredentials(key)

			By("a patcher setting inspect-complete by hand")
			annotateHost(key, InspectionRequestAnnotation, "inspect-complete")
			deploying := settlePhysicalHost(hostReconciler, key)
			Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying))
			Expect(deploying.Status.DeployingTimestamp).NotTo(BeNil())

			By("the same patcher setting the /provisioned annotation by hand")
			callbackSignals["/provisioned report"].forge(key)
			stuck := settlePhysicalHost(hostReconciler, key)
			Expect(stuck.Status.State).To(Equal(infrav1.StateDeploying), "Ready needs the inspector's bound /provisioned")
			Expect(stuck.Status.Ready).To(BeTrue(), "as staged; the reconciler wrote nothing")
			Expect(stuck.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))

			By("the inspector's /provisioned")
			reportProvisioned(key)
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateReady))
		})
	})

	Context("the handlers", func() {
		It("write the signal and its binding in one patch", func() {
			key := provisioningHost(ns.Name, "patch-host", "patch-machine", infrav1.StateDeploying, nil)
			token := ensureCallbackCredentials(key)

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
			handler := &ProvisionedHandler{Client: recording, Log: ctrl.Log.WithName("callback-binding-patch")}
			Expect(handler.signalProvisioned(ctx, handler.Log, key.Namespace, key.Name, token)).To(Succeed())

			mu.Lock()
			defer mu.Unlock()
			Expect(patches).To(HaveLen(1))
			Expect(patches[0]).To(ContainSubstring(ProvisionedRequestAnnotation))
			Expect(patches[0]).To(ContainSubstring(callbackBindingAnnotation(ProvisionedRequestAnnotation)))
		})

		It("refuse to bind a callback whose token is not the host's current one", func() {
			key := provisioningHost(ns.Name, "stale-host", "stale-machine", infrav1.StateDeploying, nil)
			ensureCallbackCredentials(key)

			By("a callback authenticated under a token that has since been replaced")
			handler := &ProvisionedHandler{Client: k8sClient, Log: ctrl.Log.WithName("callback-binding-stale")}
			Expect(handler.signalProvisioned(ctx, handler.Log, key.Namespace, key.Name, "the-previous-token")).NotTo(Succeed())
			Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))

			By("a Secret with no boot nonce, which cannot tell one boot cycle from the next")
			rewriteCredentials(key, func(data map[string][]byte) { delete(data, bootNonceSecretKey) })
			token := callbackTokenOf(key)
			Expect(handler.signalProvisioned(ctx, handler.Log, key.Namespace, key.Name, token)).To(MatchError(errCallbackBindingUnavailable))
			Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
		})

		It("leave no ConfigMap behind when the inspection report cannot be bound", func() {
			key := provisioningHost(ns.Name, "unbound-report-host", "unbound-report-machine", infrav1.StateInspecting, nil)
			ensureCallbackCredentials(key)
			handler := &InspectionHandler{Client: k8sClient, Log: ctrl.Log.WithName("callback-binding-unbound")}
			Expect(handler.processInspectionReport(ctx, handler.Log, key.Namespace, key.Name, "not-the-token",
				InspectionReportRequest{Manufacturer: "Acme"})).NotTo(Succeed())
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: inspectionResultConfigMapName(key.Name)}, &corev1.ConfigMap{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the ConfigMap is written only for a callback that can be bound")
		})
	})

	Context("the reconciler", func() {
		It("leaves a signal for the next pass when it cannot read the host's credentials", func() {
			key := provisioningHost(ns.Name, "unreadable-host", "unreadable-machine", infrav1.StateDeploying, nil)
			reportProvisioned(key)

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
				Client: flaky, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("callback-binding-unreadable"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			waiting := getPhysicalHost(key)
			Expect(waiting.Annotations).To(HaveKeyWithValue(ProvisionedRequestAnnotation, "provisioned"),
				"a read error says nothing about the binding; dropping the signal would lose a real one to an API blip")
			Expect(waiting.Annotations).To(HaveKey(callbackBindingAnnotation(ProvisionedRequestAnnotation)))
			Expect(waiting.Status.State).To(Equal(infrav1.StateDeploying))

			failing.Store(false)
			for range 3 {
				_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateReady))
		})

		It("logs a rejected signal with the host and the annotation, and no credential material", func() {
			key := provisioningHost(ns.Name, "logged-host", "logged-machine", infrav1.StateDeploying, nil)
			token := ensureCallbackCredentials(key)
			forgedValue := sanitizeFailureReason("forged reason that must not be logged")
			annotateHost(key, ProvisionFailedRequestAnnotation, forgedValue)
			binding := strings.Repeat("c", 64)
			annotateHost(key, callbackBindingAnnotation(ProvisionFailedRequestAnnotation), binding)

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
			Expect(rejection).To(ContainSubstring("logged-host"))
			Expect(rejection).To(ContainSubstring(ProvisionFailedRequestAnnotation))
			Expect(logged.String()).NotTo(ContainSubstring(token), "the bearer token is never logged")
			Expect(logged.String()).NotTo(ContainSubstring(binding), "a binding is never logged")
			Expect(logged.String()).NotTo(ContainSubstring("forged reason that must not be logged"), "an unbound value is never logged")
		})
	})
})
