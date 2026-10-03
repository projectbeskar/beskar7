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
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// Fixtures for the annotations the PhysicalHost reconciler acts on (SEC-15,
// D-034, D-037). A signal counts only with the binding its writer put next to
// it, so a spec that stages one without going through the handler or the
// Beskar7Machine controller has to bind it the way they do: with the production
// binder, built from the host's bootstrap-token Secret.

// callbackSignalKeys are the annotations a spec may stage and expect to count:
// the three the callback handlers write and the inspection request the
// Beskar7Machine controller writes.
var callbackSignalKeys = []string{InspectionResultAnnotation, ProvisionedRequestAnnotation, ProvisionFailedRequestAnnotation, InspectionRequestAnnotation}

// fixtureConsumerUID is the consumer-uid the fixture Secret records. Nothing
// resolves it: the Secret's value is what the binding covers.
const fixtureConsumerUID = "fixture-consumer-uid"

// ensureCallbackCredentials gives the claimed host a bootstrap-token Secret
// holding a bearer token, a boot nonce and a consumer binding, owned by the host
// as the manager writes it, unless it already has one, and returns the token.
func ensureCallbackCredentials(host client.ObjectKey) string {
	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: host.Namespace, Name: bootstrapTokenSecretName(host.Name)}, secret)
	if err == nil {
		return string(secret.Data[bootstrapTokenSecretKey])
	}
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "unexpected error reading the fixture credentials: %v", err)

	consumer := getPhysicalHost(host).Spec.ConsumerRef
	Expect(consumer).NotTo(BeNil(), "a callback is bound to the claim, so the host has to be claimed")
	token, _, err := auth.MintToken()
	Expect(err).NotTo(HaveOccurred())
	nonce, _, err := auth.MintToken()
	Expect(err).NotTo(HaveOccurred())
	data := boundCredentialData(consumer.Name, token, time.Hour, nonce, 10*time.Minute)
	data[bootstrapConsumerUIDSecretKey] = []byte(fixtureConsumerUID)
	putCredentialSecret(host, data)
	return token
}

// callbackTokenOf returns the bearer token in the host's bootstrap-token Secret.
func callbackTokenOf(host client.ObjectKey) string {
	return string(getCredentialSecret(host).Data[bootstrapTokenSecretKey])
}

// callbackBinderFor returns the production binder for the host as its Secret
// stands now.
func callbackBinderFor(host client.ObjectKey) *callbackBinder {
	h := getPhysicalHost(host)
	creds, _, err := boundBootstrapCredentials(ctx, k8sClient, h)
	Expect(err).NotTo(HaveOccurred())
	binder, err := newCallbackBinder(h, creds)
	Expect(err).NotTo(HaveOccurred())
	return binder
}

// bindCallbackAnnotation sets the signal annotation on the host to value, with
// the binding its writer would have put next to it. digest is contentDigest of
// the stored report for InspectionResultAnnotation, and empty for the others.
func bindCallbackAnnotation(host client.ObjectKey, annotation, value, digest string) {
	binder := callbackBinderFor(host)
	current := getPhysicalHost(host)
	bound := current.DeepCopy()
	binder.setAnnotation(bound, annotation, value, digest)
	Expect(k8sClient.Patch(ctx, bound, client.MergeFrom(current))).To(Succeed())
}

// bindCallbackAnnotations adds the binding for each signal annotation the host
// carries, creating the host's credentials if it has none: a host staged with a
// signal in its annotations looks as the handler, or the Beskar7Machine
// controller, would have left it. An inspection-result signal binds the
// report.json its ConfigMap holds.
func bindCallbackAnnotations(host client.ObjectKey) {
	staged := getPhysicalHost(host)
	var present []string
	for _, key := range callbackSignalKeys {
		if _, ok := staged.Annotations[key]; ok {
			present = append(present, key)
		}
	}
	if len(present) == 0 {
		return
	}
	ensureCallbackCredentials(host)
	for _, key := range present {
		digest := ""
		if key == InspectionResultAnnotation {
			digest = contentDigest(resultConfigMapReport(host.Namespace, staged.Annotations[key]))
		}
		bindCallbackAnnotation(host, key, staged.Annotations[key], digest)
	}
}

// resultConfigMapReport returns the report.json the named ConfigMap holds, or
// "" when there is none.
func resultConfigMapReport(namespace, name string) string {
	cm := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		return ""
	}
	return cm.Data[inspectionResultDataKey]
}

// stageCallbackBindings binds the signal annotations of a host a spec has just
// created (bindCallbackAnnotations) and reads it back into host, so the spec
// goes on from the host as the handler would have left it.
func stageCallbackBindings(host *infrav1.PhysicalHost) {
	key := client.ObjectKeyFromObject(host)
	bindCallbackAnnotations(key)
	Expect(k8sClient.Get(ctx, key, host)).To(Succeed())
}

// requestInspection sets the inspection-request annotation on the claimed host
// as the Beskar7Machine controller does: the value, signed with the host's
// credentials (created if it has none) in the same patch.
func requestInspection(host client.ObjectKey, value string) {
	ensureCallbackCredentials(host)
	bindCallbackAnnotation(host, InspectionRequestAnnotation, value, "")
}

// claimWithCredentials claims the host for the Beskar7Machine named machineName,
// as a claim does, and gives it the credentials that machine would have minted,
// so the machine controller can sign a request for it. It returns the host as
// persisted.
func claimWithCredentials(host client.ObjectKey, machineName string) *infrav1.PhysicalHost {
	setHostConsumer(host, machineName)
	ensureCallbackCredentials(host)
	return getPhysicalHost(host)
}

// ensureCredentials runs the machine controller's ensureBootstrapCredentials
// and expects it to succeed, returning the credentials it reports.
func ensureCredentials(r *Beskar7MachineReconciler, machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost, now time.Time) bootstrapCredentials {
	creds, err := r.ensureBootstrapCredentials(ctx, r.Log, machine, host, now)
	Expect(err).NotTo(HaveOccurred())
	return creds
}

// claimWithRequest claims the host for the Beskar7Machine named machineName and
// sets a signed inspection request, in one patch: a claim and a request that
// land together, so the host goes straight from Available to the state the
// request names. The credentials the machine would have minted for the claim
// replace whatever the host's Secret held (a new claim mints afresh), and the
// request is signed with them.
func claimWithRequest(host client.ObjectKey, machineName, value string) {
	token, _, err := auth.MintToken()
	Expect(err).NotTo(HaveOccurred())
	nonce, _, err := auth.MintToken()
	Expect(err).NotTo(HaveOccurred())
	data := boundCredentialData(machineName, token, time.Hour, nonce, 10*time.Minute)
	data[bootstrapConsumerUIDSecretKey] = []byte(fixtureConsumerUID)
	putCredentialSecret(host, data)

	current := getPhysicalHost(host)
	claimed := current.DeepCopy()
	claimed.Spec.ConsumerRef = &corev1.ObjectReference{
		Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
		Name: machineName, Namespace: host.Namespace,
	}
	binder, err := newCallbackBinder(claimed, readBootstrapCredentials(getCredentialSecret(host)))
	Expect(err).NotTo(HaveOccurred())
	binder.setAnnotation(claimed, InspectionRequestAnnotation, value, "")
	Expect(k8sClient.Patch(ctx, claimed, client.MergeFrom(current))).To(Succeed())
}
