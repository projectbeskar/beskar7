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
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

var _ = Describe("Cleaning credentials on a claimed host", func() {
	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cleaning-purpose-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	// markForCleaning turns the host's claim credentials into a cleaning run's, keeping the consumer,
	// so only the purpose tells them apart.
	markForCleaning := func(key client.ObjectKey) {
		secret := getCredentialSecret(key)
		secret.Data[bootstrapPurposeSecretKey] = []byte(credentialPurposeClean)
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
	}

	It("refuses them on the claim routes, even when they name the host's consumer", func() {
		key := provisioningHost(ns.Name, "purpose-claim", "purpose-claim-machine", infrav1.StateDeploying, nil)
		token := ensureCallbackCredentials(key)
		markForCleaning(key)

		_, _, err := boundBootstrapCredentials(ctx, k8sClient, getPhysicalHost(key))
		Expect(err).To(MatchError(ContainSubstring("not a claim")))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap/"+key.Namespace+"/"+key.Name, nil)
		req.SetPathValue("namespace", key.Namespace)
		req.SetPathValue("hostName", key.Name)
		Expect(newBearerTokenVerifier(k8sClient, ctrl.Log)(token, req)).NotTo(Succeed())
	})

	It("are replaced with fresh claim credentials when the machine mints", func() {
		const machineName = "purpose-remint-machine"
		key := provisioningHost(ns.Name, "purpose-remint", machineName, infrav1.StateInUse, nil)
		cleaningToken := ensureCallbackCredentials(key)
		markForCleaning(key)
		machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
			Name: machineName, Namespace: ns.Name, UID: types.UID(fixtureConsumerUID),
		}}
		r := &Beskar7MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Log: ctrl.Log}

		creds := ensureCredentials(r, machine, getPhysicalHost(key), time.Now())

		Expect(creds.purpose).To(Equal(credentialPurposeClaim))
		Expect(creds.token).NotTo(Equal(cleaningToken), "a cleaning run's token never carries into a claim")
		stored := readBootstrapCredentials(getCredentialSecret(key))
		Expect(stored.forCleaning()).To(BeFalse())
		Expect(stored.consumer).To(Equal(machineName))
	})
})
