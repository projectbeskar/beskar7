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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// Callback credentials used to be read from PhysicalHost.Status.Bootstrap, and
// the PhysicalHost controller copied whatever hash and expiry the
// bootstrap-token and boot-nonce annotations carried into it. Anyone who could
// patch a PhysicalHost but not read Secrets could therefore mint a bearer
// token of their choosing (one that never expired, if the annotation had no
// expiry), or a boot nonce that made /boot hand out the host's real bearer
// token (SEC-12). The host's claim was not checked either: a token for one
// consumer fetched whatever consumer ConsumerRef named next, and a token
// reused across a release and re-claim fetched the next claim's bootstrap
// data (SEC-13). These specs pin D-029: the per-host bootstrap-token Secret,
// bound to the claiming Beskar7Machine by name, is the only thing the
// callbacks check.

// credentialTime is the form the bootstrap-token Secret carries its expiries in.
func credentialTime(t time.Time) []byte {
	return []byte(t.UTC().Format(time.RFC3339))
}

// boundCredentialData returns the data of a bootstrap-token Secret that binds
// token and nonce to the Beskar7Machine named consumer. An empty token or nonce
// is left out, and so is its expiry.
func boundCredentialData(consumer, token string, tokenTTL time.Duration, nonce string, nonceTTL time.Duration) map[string][]byte {
	now := time.Now()
	data := map[string][]byte{bootstrapConsumerSecretKey: []byte(consumer)}
	if token != "" {
		data[bootstrapTokenSecretKey] = []byte(token)
		data[bootstrapTokenExpiresAtSecretKey] = credentialTime(now.Add(tokenTTL))
	}
	if nonce != "" {
		data[bootNonceSecretKey] = []byte(nonce)
		data[bootNonceExpiresAtSecretKey] = credentialTime(now.Add(nonceTTL))
	}
	return data
}

// credentialSecret returns the bootstrap-token Secret of the host named
// hostName holding data, for fake clients.
func credentialSecret(namespace, hostName string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: bootstrapTokenSecretName(hostName), Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// putCredentialSecret creates the host's bootstrap-token Secret with data, or
// replaces the data of the one it has.
func putCredentialSecret(host client.ObjectKey, data map[string][]byte) {
	key := client.ObjectKey{Namespace: host.Namespace, Name: bootstrapTokenSecretName(host.Name)}
	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		Expect(k8sClient.Create(ctx, credentialSecret(host.Namespace, host.Name, data))).To(Succeed())
		return
	}
	Expect(err).NotTo(HaveOccurred())
	secret.Data = data
	Expect(k8sClient.Update(ctx, secret)).To(Succeed())
}

// getCredentialSecret returns the host's bootstrap-token Secret as persisted.
func getCredentialSecret(host client.ObjectKey) *corev1.Secret {
	secret := &corev1.Secret{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: host.Namespace, Name: bootstrapTokenSecretName(host.Name)}, secret)).To(Succeed())
	return secret
}

// setHostConsumer points the host's ConsumerRef at the Beskar7Machine named
// machineName in the host's namespace, as a claim does.
func setHostConsumer(host client.ObjectKey, machineName string) {
	current := getPhysicalHost(host)
	claimed := current.DeepCopy()
	claimed.Spec.ConsumerRef = &corev1.ObjectReference{
		Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
		Name: machineName, Namespace: host.Namespace,
	}
	Expect(k8sClient.Patch(ctx, claimed, client.MergeFrom(current))).To(Succeed())
}

// annotateHost sets one annotation on the host, as anyone allowed to patch
// PhysicalHosts can.
func annotateHost(host client.ObjectKey, annotation, value string) {
	current := getPhysicalHost(host)
	annotated := current.DeepCopy()
	if annotated.Annotations == nil {
		annotated.Annotations = map[string]string{}
	}
	annotated.Annotations[annotation] = value
	Expect(k8sClient.Patch(ctx, annotated, client.MergeFrom(current))).To(Succeed())
}

// consumerWithBootstrapData creates a Beskar7Machine owned by a CAPI Machine
// whose bootstrap data Secret holds marker, and returns both as persisted.
func consumerWithBootstrapData(namespace, name, marker string) (*infrav1.Beskar7Machine, *clusterv1.Machine) {
	dataSecret := name + "-bootstrap-data"
	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dataSecret, Namespace: namespace},
		Data:       map[string][]byte{bootstrapDataSecretKey: []byte("#cloud-config\n# " + marker + "\n")},
	})).To(Succeed())
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: clusterv1.MachineSpec{
			ClusterName: "callback-credentials-cluster",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: name,
			},
			Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To(dataSecret)},
		},
	}
	Expect(k8sClient.Create(ctx, machine)).To(Succeed())
	b7m := &infrav1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
				Name: machine.Name, UID: machine.UID,
			}},
		},
		Spec: infrav1.Beskar7MachineSpec{
			InspectionImageURL: "https://boot.example.com/inspect",
			TargetImageURL:     "https://boot.example.com/kairos.raw",
			TargetImageDigest:  bootTestDigest,
		},
	}
	Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
	return b7m, machine
}

// newCallbackTestServer serves the bearer-gated inspection and bootstrap
// routes and the nonce-gated /boot route the way SetupCallbackServer wires
// them, below TLS.
func newCallbackTestServer() *httptest.Server {
	log := ctrl.Log.WithName("callback-credentials")
	verifier := newBearerTokenVerifier(k8sClient, log)
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/inspection/{namespace}/{hostName}",
		auth.RequireBearer(log, verifier, &InspectionHandler{Client: k8sClient, Log: log}))
	mux.Handle("GET /api/v1/bootstrap/{namespace}/{hostName}",
		auth.RequireBearer(log, verifier, &BootstrapHandler{Client: k8sClient, Log: log}))
	mux.Handle("GET /api/v1/boot/{namespace}/{hostName}/{nonce}",
		&BootHandler{Client: k8sClient, Log: log, Config: bootTestConfig()})
	return httptest.NewServer(mux)
}

// callbackRequest sends one request with a bearer token and returns the status
// code and body.
func callbackRequest(method, url, token string) (int, string) {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"manufacturer":"Acme","cpus":[{"id":"cpu0","cores":8}]}`)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	return resp.StatusCode, readBody(resp)
}

var _ = Describe("Callback credentials come only from the host's bootstrap-token Secret (SEC-12, D-029)", func() {
	var (
		ns       *corev1.Namespace
		server   *httptest.Server
		hostR    *PhysicalHostReconciler
		machineR *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "callback-credentials-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		server = newCallbackTestServer()
		hostR = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("callback-credentials-host"),
			Recorder:             record.NewFakeRecorder(100),
			RedfishClientFactory: reachableBMC(),
		}
		machineR = &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("callback-credentials-machine"),
			RedfishClientFactory: reachableBMC(),
			BootstrapURLBase:     "https://callback.example.com:8082",
		}
	})

	AfterEach(func() {
		server.Close()
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	// reconcileHost runs the PhysicalHost reconciler on the host n times: the
	// annotation handoffs this suite exercises take up to two passes.
	reconcileHost := func(key client.ObjectKey, n int) *infrav1.PhysicalHost {
		for range n {
			_, err := hostR.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		return getPhysicalHost(key)
	}
	mint := func() (string, string) {
		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		return plaintext, hash
	}
	inspectionURL := func(key client.ObjectKey) string {
		return fmt.Sprintf("%s/api/v1/inspection/%s/%s", server.URL, key.Namespace, key.Name)
	}
	bootstrapURL := func(key client.ObjectKey) string {
		return fmt.Sprintf("%s/api/v1/bootstrap/%s/%s", server.URL, key.Namespace, key.Name)
	}
	// mintFor has machine mint the host's credentials through
	// triggerInspection and the host take up what it signalled, and returns the
	// bearer token and boot nonce the inspector would boot with.
	mintFor := func(machine *infrav1.Beskar7Machine, key client.ObjectKey) (string, string) {
		_, err := machineR.triggerInspection(ctx, machineR.Log, machine, getPhysicalHost(key))
		Expect(err).NotTo(HaveOccurred())
		reconcileHost(key, 2)
		secret := getCredentialSecret(key)
		return string(secret.Data[bootstrapTokenSecretKey]), string(secret.Data[bootNonceSecretKey])
	}

	DescribeTable("a forged bootstrap-token annotation never authenticates a callback",
		func(withExpiry bool) {
			victim, _ := consumerWithBootstrapData(ns.Name, "victim-machine", "VICTIM-BOOTSTRAP-DATA")
			key := provisioningHost(ns.Name, "forged-token-host", victim.Name, infrav1.StateInUse, nil)
			realToken, _ := mintFor(victim, key)

			By("someone who can patch the host but not read its Secret annotating a token of their own")
			attackerToken, attackerHash := mint()
			forged := map[string]string{"hash": attackerHash}
			if withExpiry {
				forged["issuedAt"] = time.Now().UTC().Format(time.RFC3339)
				forged["expiresAt"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			}
			value, err := json.Marshal(forged)
			Expect(err).NotTo(HaveOccurred())
			annotateHost(key, BootstrapTokenAnnotation, string(value))
			host := reconcileHost(key, 3)

			code, _ := callbackRequest(http.MethodPost, inspectionURL(key), attackerToken)
			Expect(code).To(Equal(http.StatusUnauthorized), "the attacker's own token must not post an inspection report")
			code, body := callbackRequest(http.MethodGet, bootstrapURL(key), attackerToken)
			Expect(code).To(Equal(http.StatusUnauthorized), "the attacker's own token must not fetch bootstrap data")
			Expect(body).NotTo(ContainSubstring("VICTIM-BOOTSTRAP-DATA"))
			Expect(host.Annotations).NotTo(HaveKey(BootstrapTokenAnnotation), "the retired annotation is removed")

			By("the token the machine minted still working")
			code, body = callbackRequest(http.MethodGet, bootstrapURL(key), realToken)
			Expect(code).To(Equal(http.StatusOK))
			Expect(body).To(ContainSubstring("VICTIM-BOOTSTRAP-DATA"))
		},
		Entry("with an expiry", true),
		Entry("without an expiry, which the status check treated as never expiring", false),
	)

	It("a forged boot-nonce annotation never boots the host, and /boot never hands out the real token for it", func() {
		victim, _ := consumerWithBootstrapData(ns.Name, "victim-machine", "VICTIM-BOOTSTRAP-DATA")
		key := provisioningHost(ns.Name, "forged-nonce-host", victim.Name, infrav1.StateInUse, nil)
		realToken, realNonce := mintFor(victim, key)
		Expect(realToken).NotTo(BeEmpty())

		By("annotating a nonce the attacker chose")
		attackerNonce, attackerHash := mint()
		value, err := json.Marshal(map[string]string{
			"hash":      attackerHash,
			"expiresAt": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		})
		Expect(err).NotTo(HaveOccurred())
		annotateHost(key, BootNonceAnnotation, string(value))
		host := reconcileHost(key, 3)

		resp := doBoot(server.URL, key.Namespace, key.Name, attackerNonce)
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound), "the forged nonce must get the opaque failure")
		Expect(body).To(Equal(bootHandlerOpaqueFailureBody + "\n"))
		Expect(body).NotTo(ContainSubstring(realToken), "the real bearer token must never be served for a forged nonce")
		Expect(host.Annotations).NotTo(HaveKey(BootNonceAnnotation), "the retired annotation is removed")

		By("the nonce the machine minted still booting the host")
		resp = doBoot(server.URL, key.Namespace, key.Name, realNonce)
		body = readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("beskar7.token=" + realToken))
	})

	It("a token minted for one consumer never fetches the bootstrap data of the consumer ConsumerRef names next", func() {
		holder, _ := consumerWithBootstrapData(ns.Name, "machine-x", "X-BOOTSTRAP-DATA")
		other, _ := consumerWithBootstrapData(ns.Name, "machine-m", "M-BOOTSTRAP-DATA")
		key := provisioningHost(ns.Name, "repointed-host", holder.Name, infrav1.StateInUse, nil)
		token, _ := mintFor(holder, key)

		code, body := callbackRequest(http.MethodGet, bootstrapURL(key), token)
		Expect(code).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("X-BOOTSTRAP-DATA"))

		By("re-pointing the host's ConsumerRef at another machine in the namespace")
		setHostConsumer(key, other.Name)

		code, body = callbackRequest(http.MethodGet, bootstrapURL(key), token)
		Expect(body).NotTo(ContainSubstring("M-BOOTSTRAP-DATA"), "the token was minted for machine-x, not machine-m")
		Expect(code).To(Equal(http.StatusUnauthorized))

		By("the handler refusing it too, should anything let the request through")
		req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap/"+key.Namespace+"/"+key.Name, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.SetPathValue("namespace", key.Namespace)
		req.SetPathValue("hostName", key.Name)
		w := httptest.NewRecorder()
		(&BootstrapHandler{Client: k8sClient, Log: ctrl.Log.WithName("callback-credentials-direct")}).ServeHTTP(w, req)
		Expect(w.Code).To(Equal(http.StatusNotFound))
		Expect(w.Body.String()).NotTo(ContainSubstring("M-BOOTSTRAP-DATA"))
	})

	It("a token from an earlier claim never fetches the next claim's bootstrap data", func() {
		first, _ := consumerWithBootstrapData(ns.Name, "machine-x", "X-BOOTSTRAP-DATA")
		next, _ := consumerWithBootstrapData(ns.Name, "machine-y", "Y-BOOTSTRAP-DATA")
		key := provisioningHost(ns.Name, "reclaimed-host", first.Name, infrav1.StateInUse, nil)
		firstToken, _ := mintFor(first, key)
		code, _ := callbackRequest(http.MethodGet, bootstrapURL(key), firstToken)
		Expect(code).To(Equal(http.StatusOK))

		By("releasing the host and letting the next machine claim it")
		releasePhysicalHost(key)
		Expect(reconcileHost(key, 2).Status.State).To(Equal(infrav1.StateAvailable))
		setHostConsumer(key, next.Name)
		Expect(reconcileHost(key, 2).Status.State).To(Equal(infrav1.StateInUse))
		nextToken, _ := mintFor(next, key)

		code, body := callbackRequest(http.MethodGet, bootstrapURL(key), firstToken)
		Expect(body).NotTo(ContainSubstring("Y-BOOTSTRAP-DATA"), "the first claim's token must not open the next claim's data")
		Expect(code).To(Equal(http.StatusUnauthorized))
		Expect(nextToken).NotTo(Equal(firstToken), "a re-claim by another machine mints a fresh token")

		code, body = callbackRequest(http.MethodGet, bootstrapURL(key), nextToken)
		Expect(code).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("Y-BOOTSTRAP-DATA"))
	})

	It("rejects every callback for a host nobody has claimed", func() {
		holder, _ := consumerWithBootstrapData(ns.Name, "machine-x", "X-BOOTSTRAP-DATA")
		key := provisioningHost(ns.Name, "unclaimed-host", holder.Name, infrav1.StateInUse, nil)
		token, nonce := mintFor(holder, key)
		releasePhysicalHost(key)

		code, _ := callbackRequest(http.MethodPost, inspectionURL(key), token)
		Expect(code).To(Equal(http.StatusUnauthorized))
		code, _ = callbackRequest(http.MethodGet, bootstrapURL(key), token)
		Expect(code).To(Equal(http.StatusUnauthorized))
		resp := doBoot(server.URL, key.Namespace, key.Name, nonce)
		Expect(readBody(resp)).NotTo(ContainSubstring(token))
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	// D-024 was a second mint computed from a host that did not yet show the
	// first: the inspector already held the first token, the second replaced
	// it, and every callback was rejected. With the Secret the only source of
	// truth, a stale host cannot cause a mint, and a stale Secret cannot
	// overwrite one: its write carries the resourceVersion it was read at.
	It("a second mint computed from a stale view never replaces the token already handed out", func() {
		machine, _ := consumerWithBootstrapData(ns.Name, "machine-m", "M-BOOTSTRAP-DATA")
		key := provisioningHost(ns.Name, "double-mint-host", machine.Name, infrav1.StateInUse, nil)
		By("the Secret an earlier claim of the host left behind")
		previousToken, _ := mint()
		putCredentialSecret(key, boundCredentialData("machine-previous", previousToken, -time.Minute, "", 0))
		staleSecret := getCredentialSecret(key)

		token, _ := mintFor(machine, key)
		code, _ := callbackRequest(http.MethodGet, bootstrapURL(key), token)
		Expect(code).To(Equal(http.StatusOK))

		By("a reconcile reading the host in the D-024 window: InUse, and no credential visible on it")
		staleHost := getPhysicalHost(key)
		staleHost.Status.State = infrav1.StateInUse
		staleHost.Status.Bootstrap = nil
		delete(staleHost.Annotations, BootstrapTokenAnnotation)
		delete(staleHost.Annotations, BootNonceAnnotation)
		_, err := machineR.triggerInspection(ctx, machineR.Log, machine, staleHost)
		Expect(err).NotTo(HaveOccurred())
		reconcileHost(key, 2)
		Expect(string(getCredentialSecret(key).Data[bootstrapTokenSecretKey])).To(Equal(token),
			"the token the inspector already holds must stay")
		code, _ = callbackRequest(http.MethodGet, bootstrapURL(key), token)
		Expect(code).To(Equal(http.StatusOK), "exactly the first token verifies")

		By("a reconcile working from the Secret as it was before the first mint")
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		secretKey := client.ObjectKeyFromObject(staleSecret)
		staleReader := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if s, ok := obj.(*corev1.Secret); ok && k == secretKey {
					staleSecret.DeepCopyInto(s)
					return nil
				}
				return c.Get(ctx, k, obj, opts...)
			},
		})
		staleR := &Beskar7MachineReconciler{
			Client: staleReader, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("callback-credentials-stale"),
			RedfishClientFactory: reachableBMC(),
		}
		_, err = staleR.triggerInspection(ctx, staleR.Log, machine, getPhysicalHost(key))
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "a mint from a stale Secret must fail and be retried, got: %v", err)
		Expect(string(getCredentialSecret(key).Data[bootstrapTokenSecretKey])).To(Equal(token))
		code, _ = callbackRequest(http.MethodGet, bootstrapURL(key), token)
		Expect(code).To(Equal(http.StatusOK))
	})

	It("mirrors the Secret into status, follows a change to it, and removes the retired annotations without promoting them", func() {
		key := provisioningHost(ns.Name, "mirror-host", "mirror-machine", infrav1.StateInspecting, nil)
		token, _ := mint()
		nonce, _ := mint()
		putCredentialSecret(key, boundCredentialData("mirror-machine", token, time.Hour, nonce, 10*time.Minute))

		By("annotating credentials that are not the Secret's")
		_, forgedTokenHash := mint()
		_, forgedNonceHash := mint()
		expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		forgedToken, err := json.Marshal(map[string]string{"hash": forgedTokenHash, "issuedAt": expiry, "expiresAt": expiry})
		Expect(err).NotTo(HaveOccurred())
		forgedNonce, err := json.Marshal(map[string]string{"hash": forgedNonceHash, "expiresAt": expiry})
		Expect(err).NotTo(HaveOccurred())
		annotateHost(key, BootstrapTokenAnnotation, string(forgedToken))
		annotateHost(key, BootNonceAnnotation, string(forgedNonce))

		host := reconcileHost(key, 1)
		Expect(host.Annotations).NotTo(HaveKey(BootstrapTokenAnnotation), "removed on sight, in the first pass")
		Expect(host.Annotations).NotTo(HaveKey(BootNonceAnnotation), "removed on sight, in the first pass")
		Expect(host.Status.Bootstrap).NotTo(BeNil())
		Expect(host.Status.Bootstrap.TokenHash).NotTo(Equal(forgedTokenHash), "an annotation is never promoted")
		Expect(host.Status.Bootstrap.BootNonceHash).NotTo(Equal(forgedNonceHash), "an annotation is never promoted")
		Expect(auth.Verify(token, host.Status.Bootstrap.TokenHash)).To(BeTrue(), "status mirrors the Secret's token")
		Expect(auth.Verify(nonce, host.Status.Bootstrap.BootNonceHash)).To(BeTrue(), "status mirrors the Secret's nonce")
		Expect(host.Status.Bootstrap.ExpiresAt).NotTo(BeNil())
		Expect(host.Status.Bootstrap.ExpiresAt.Time).To(BeTemporally("~", time.Now().Add(time.Hour), 5*time.Second))
		Expect(host.Status.Bootstrap.BootNonceExpiresAt).NotTo(BeNil())
		Expect(host.Status.Bootstrap.BootNonceExpiresAt.Time).To(BeTemporally("~", time.Now().Add(10*time.Minute), 5*time.Second))

		By("the Secret changing, which wakes the host")
		Expect(hostR.SecretToPhysicalHosts(ctx, getCredentialSecret(key))).To(ContainElement(reconcile.Request{NamespacedName: key}))
		newer, _ := mint()
		putCredentialSecret(key, boundCredentialData("mirror-machine", newer, 2*time.Hour, "", 0))
		host = reconcileHost(key, 1)
		Expect(auth.Verify(newer, host.Status.Bootstrap.TokenHash)).To(BeTrue(), "the mirror follows the Secret")
		Expect(host.Status.Bootstrap.ExpiresAt.Time).To(BeTemporally("~", time.Now().Add(2*time.Hour), 5*time.Second))
		Expect(host.Status.Bootstrap.BootNonceHash).To(BeEmpty(), "a nonce the Secret no longer holds is not mirrored")
	})

	// Upgrade from a release that promoted the annotations: the Secret holds
	// only the plaintexts, and the hash and lifetime are in status. A run in
	// flight keeps its credentials for one release; a host already Ready does
	// not, since no callback follows Ready.
	Context("a host whose Secret predates the consumer binding", func() {
		legacyHost := func(name, machineName, state string) (client.ObjectKey, string) {
			key := provisioningHost(ns.Name, name, machineName, state, nil)
			token, tokenHash := mint()
			nonce, nonceHash := mint()
			putCredentialSecret(key, map[string][]byte{
				bootstrapTokenSecretKey: []byte(token),
				bootNonceSecretKey:      []byte(nonce),
			})
			host := getPhysicalHost(key)
			issuedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
			expiresAt := metav1.NewTime(time.Now().Add(50 * time.Minute))
			nonceExpiresAt := metav1.NewTime(time.Now().Add(5 * time.Minute))
			host.Status.Bootstrap = &infrav1.BootstrapStatus{
				TokenHash: tokenHash, IssuedAt: &issuedAt, ExpiresAt: &expiresAt,
				BootNonceHash: nonceHash, BootNonceExpiresAt: &nonceExpiresAt,
			}
			Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			return key, token
		}
		reconcileMachine := func(b7m *infrav1.Beskar7Machine, machine *clusterv1.Machine) {
			b7m.Finalizers = []string{Beskar7MachineFinalizer}
			_, err := machineR.reconcileNormal(ctx, machineR.Log, b7m, machine)
			Expect(err).NotTo(HaveOccurred())
		}

		It("keeps an Inspecting host's token working by binding it to its machine", func() {
			b7m, machine := consumerWithBootstrapData(ns.Name, "inflight-machine", "INFLIGHT-BOOTSTRAP-DATA")
			key, token := legacyHost("inflight-host", b7m.Name, infrav1.StateInspecting)
			statusExpiry := getPhysicalHost(key).Status.Bootstrap.ExpiresAt.Time

			reconcileMachine(b7m, machine)

			code, body := callbackRequest(http.MethodGet, bootstrapURL(key), token)
			Expect(code).To(Equal(http.StatusOK))
			Expect(body).To(ContainSubstring("INFLIGHT-BOOTSTRAP-DATA"))
			secret := getCredentialSecret(key)
			Expect(string(secret.Data[bootstrapTokenSecretKey])).To(Equal(token), "the backfill keeps the token")
			Expect(string(secret.Data[bootstrapConsumerSecretKey])).To(Equal(b7m.Name))
			expiry, err := time.Parse(time.RFC3339, string(secret.Data[bootstrapTokenExpiresAtSecretKey]))
			Expect(err).NotTo(HaveOccurred())
			Expect(expiry).To(BeTemporally("<=", statusExpiry), "the backfilled expiry never outlives the one status advertised")
		})

		It("backfills nothing on a Ready host, whose pre-upgrade token stops working", func() {
			b7m, machine := consumerWithBootstrapData(ns.Name, "ready-machine", "READY-BOOTSTRAP-DATA")
			key, token := legacyHost("ready-host", b7m.Name, infrav1.StateReady)

			reconcileMachine(b7m, machine)

			code, body := callbackRequest(http.MethodGet, bootstrapURL(key), token)
			Expect(body).NotTo(ContainSubstring("READY-BOOTSTRAP-DATA"))
			Expect(code).To(Equal(http.StatusUnauthorized))
			Expect(getCredentialSecret(key).Data).NotTo(HaveKey(bootstrapConsumerSecretKey))
		})
	})
})
