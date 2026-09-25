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
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// A reconcile that consumes an annotation deletes it from its copy of the host.
// When that key was the host's last annotation, the copy's map is empty, the
// JSON drops it (omitempty), and the merge patch the deferred patch.Helper sends
// is "annotations": null — which deletes every annotation on the server,
// including one another writer added after this reconcile read the host. The
// inspection handler's inspection-result annotation lost exactly that race:
// the host never read the report and the run timed out (FLAKE-1).
var _ = Describe("A PhysicalHost reconcile that clears the host's last annotation", func() {
	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "annotation-clear-race-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	It("keeps an annotation another writer added in the meantime", func() {
		// A retired credential annotation (D-029), which every pass removes on
		// sight: here the host's only annotation.
		leftover := `{"hash":"` + strings.Repeat("a", 64) + `","expiresAt":"` +
			time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339) + `"}`
		key := provisioningHost(ns.Name, "race-host", "race-machine", infrav1.StateInspecting,
			map[string]string{BootNonceAnnotation: leftover})

		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		var handlerWrote atomic.Bool
		racing := interceptor.NewClient(base, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, isHost := obj.(*infrav1.PhysicalHost); isHost && handlerWrote.CompareAndSwap(false, true) {
					// The inspection handler's write, landing after the reconcile
					// read the host and before its metadata patch.
					current := getPhysicalHost(key)
					annotated := current.DeepCopy()
					annotated.Annotations[InspectionResultAnnotation] = "race-host-inspection-result"
					Expect(k8sClient.Patch(ctx, annotated, client.MergeFrom(current))).To(Succeed())
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		reconciler := &PhysicalHostReconciler{
			Client: racing, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("annotation-clear-race"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: reachableBMC(),
		}
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(handlerWrote.Load()).To(BeTrue(), "the reconcile must patch the host's metadata for this spec to mean anything")

		after := getPhysicalHost(key)
		Expect(after.Annotations).NotTo(HaveKey(BootNonceAnnotation), "the consumed annotation is cleared")
		Expect(after.Annotations).To(HaveKeyWithValue(InspectionResultAnnotation, "race-host-inspection-result"),
			"clearing one key must not delete an annotation this reconcile never read")
	})

	It("keeps it when the provision-failed handler clears a stale report", func() {
		// Claimed and in an Error that did not interrupt a deployment: the
		// handler clears the stale report instead of setting one.
		key := provisioningHost(ns.Name, "stale-report-host", "stale-report-machine", infrav1.StateError,
			map[string]string{ProvisionFailedRequestAnnotation: "stale report"})

		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		var otherWrote atomic.Bool
		racing := interceptor.NewClient(base, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, isHost := obj.(*infrav1.PhysicalHost); isHost && otherWrote.CompareAndSwap(false, true) {
					current := getPhysicalHost(key)
					annotated := current.DeepCopy()
					annotated.Annotations[InspectionRequestAnnotation] = "inspect"
					Expect(k8sClient.Patch(ctx, annotated, client.MergeFrom(current))).To(Succeed())
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		handler := &ProvisionFailedHandler{Client: racing, Log: ctrl.Log.WithName("annotation-clear-race-handler")}
		Expect(handler.signalProvisionFailed(ctx, handler.Log, key.Namespace, key.Name, "second report")).To(Succeed())
		Expect(otherWrote.Load()).To(BeTrue(), "the handler must patch the host for this spec to mean anything")

		after := getPhysicalHost(key)
		Expect(after.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation), "the stale report is cleared")
		Expect(after.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"),
			"clearing one key must not delete an annotation the handler never read")
	})
})
