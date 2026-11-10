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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// The full callback chain for a host in an admin namespace claimed through a
// pool by a machine in a tenant namespace: bearer verifier, consumer
// resolution, token binding, owner Machine, bootstrap data in the tenant namespace.
var _ = Describe("Bootstrap GET for a host claimed through a PhysicalHostPool", func() {
	const (
		grantLabel = "beskar7-pool-test"
		tenantData = "#cloud-config\nTENANT-DATA\n"
	)
	var (
		hostNs, tenantNs *corev1.Namespace
		pool             *infrav1.PhysicalHostPool
		host             *infrav1.PhysicalHost
		b7machine        *infrav1.Beskar7Machine
		server           *httptest.Server
		token            string
	)

	BeforeEach(func() {
		EnableHostPools(true)
		DeferCleanup(func() { EnableHostPools(false) })

		hostNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pool-bootstrap-hosts-"}}
		Expect(k8sClient.Create(ctx, hostNs)).To(Succeed())
		tenantNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pool-bootstrap-tenant-"}}
		Expect(k8sClient.Create(ctx, tenantNs)).To(Succeed())
		tenantNs.Labels = map[string]string{grantLabel: tenantNs.Name}
		Expect(k8sClient.Update(ctx, tenantNs)).To(Succeed())

		pool = &infrav1.PhysicalHostPool{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "pool-bootstrap-"},
			Spec: infrav1.PhysicalHostPoolSpec{
				HostNamespaces:    []string{hostNs.Name},
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{grantLabel: tenantNs.Name}},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		b7machine = testPoolMachine("pool-bootstrap-b7m", tenantNs.Name, pool.Name)
		b7machine.UID = ""
		Expect(k8sClient.Create(ctx, b7machine)).To(Succeed())

		owner := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-bootstrap-machine", Namespace: tenantNs.Name},
			Spec: clusterv1.MachineSpec{
				ClusterName:       "fake-cluster",
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "Beskar7Machine", Name: b7machine.Name},
				Bootstrap:         clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KairosConfig", Name: "fixture"}},
			},
		}
		Expect(k8sClient.Create(ctx, owner)).To(Succeed())
		dataSecret := "pool-bootstrap-data"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: dataSecret, Namespace: tenantNs.Name},
			Data:       map[string][]byte{bootstrapDataSecretKey: []byte(tenantData)},
		})).To(Succeed())
		owner.Spec.Bootstrap.DataSecretName = &dataSecret
		Expect(k8sClient.Update(ctx, owner)).To(Succeed())
		b7machine.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: owner.Name, UID: owner.UID,
		}}
		Expect(k8sClient.Update(ctx, b7machine)).To(Succeed())

		host = testPoolHost("pool-bootstrap-host", hostNs.Name, pool.Name)
		host.Spec.RedfishConnection = infrav1.RedfishConnection{Address: "https://192.0.2.30", CredentialsSecretRef: "irrelevant"}
		host.Status = infrav1.PhysicalHostStatus{}
		claimedByMachine(host, b7machine)
		Expect(k8sClient.Create(ctx, host)).To(Succeed())

		var err error
		token, _, err = auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		data := boundCredentialData(b7machine.Name, token, 30*time.Minute, "", 0)
		data[bootstrapConsumerNamespaceSecretKey] = []byte(tenantNs.Name)
		putCredentialSecret(client.ObjectKeyFromObject(host), data)

		server = httptest.NewServer(buildBootstrapMux())
		DeferCleanup(func() {
			server.Close()
			Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, hostNs)).To(Succeed())
			Expect(k8sClient.Delete(ctx, tenantNs)).To(Succeed())
		})
	})

	get := func() (int, string) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/bootstrap/%s/%s", server.URL, host.Namespace, host.Name), nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return resp.StatusCode, string(body)
	}

	It("serves the tenant's bootstrap data while the pool holds the host and grants the namespace", func() {
		Eventually(func() int { code, _ := get(); return code }).Should(Equal(http.StatusOK))
		_, body := get()
		Expect(body).To(Equal(tenantData))
	})

	It("refuses the host once its namespace leaves the pool", func() {
		Eventually(func() int { code, _ := get(); return code }).Should(Equal(http.StatusOK))
		p := &infrav1.PhysicalHostPool{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), p)).To(Succeed())
		p.Spec.HostNamespaces = []string{"elsewhere"}
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		Eventually(func() int { code, _ := get(); return code }).Should(Equal(http.StatusUnauthorized))
		_, body := get()
		Expect(body).NotTo(ContainSubstring("TENANT-DATA"))
	})

	It("refuses the host once the pool stops granting the namespace", func() {
		Eventually(func() int { code, _ := get(); return code }).Should(Equal(http.StatusOK))
		ns := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tenantNs), ns)).To(Succeed())
		delete(ns.Labels, grantLabel)
		Expect(k8sClient.Update(ctx, ns)).To(Succeed())
		Eventually(func() int { code, _ := get(); return code }).Should(Equal(http.StatusUnauthorized))
	})

	It("refuses credentials minted for a same-named machine in another namespace", func() {
		data := boundCredentialData(b7machine.Name, token, 30*time.Minute, "", 0)
		putCredentialSecret(client.ObjectKeyFromObject(host), data) // no consumer-namespace: the host's own
		Eventually(func() int { code, _ := get(); return code }).Should(Equal(http.StatusUnauthorized))
	})

	It("refuses the host while host pools are disabled", func() {
		EnableHostPools(false)
		Eventually(func() int { code, _ := get(); return code }).Should(Equal(http.StatusUnauthorized))
	})
})
