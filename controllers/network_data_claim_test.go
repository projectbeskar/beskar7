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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const (
	networkDataStatusMachine = "b7m-01"
	networkDataStatusToken   = "status-token-0123456789abcdef"
)

// networkDataStatusCase is one state of the host, its credentials and its
// network data Secret, and the status the handler must answer for it.
type networkDataStatusCase struct {
	name string
	// claimedBy is the machine the host's claim names; the credentials are
	// always bound to networkDataStatusMachine.
	claimedBy string
	noHost    bool
	secret    *corev1.Secret
	// failGet makes a Get of an object of this type fail with an API error.
	failGet client.Object
	want    int
}

// serveNetworkDataStatus calls NetworkDataHandler past the bearer middleware,
// so the handler's own checks decide the answer.
func serveNetworkDataStatus(t *testing.T, tc networkDataStatusCase) int {
	t.Helper()
	scheme := newSchemeForTest(t)
	host := networkDataTestHostWithRef(networkDataTestSecret)
	host.UID = types.UID("status-host-uid")
	host.Spec.ConsumerRef = &corev1.ObjectReference{
		Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
		Name: tc.claimedBy, Namespace: networkDataTestNamespace,
	}
	credentials := unownedCredentialSecret(networkDataTestNamespace, networkDataTestHost,
		boundCredentialData(networkDataStatusMachine, networkDataStatusToken, time.Hour, "", 0))
	if err := controllerutil.SetControllerReference(host, credentials, scheme); err != nil {
		t.Fatalf("own credentials Secret: %v", err)
	}
	objects := []client.Object{credentials}
	if !tc.noHost {
		objects = append(objects, host)
	}
	if tc.secret != nil {
		objects = append(objects, tc.secret)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if tc.failGet != nil && key.Name == tc.failGet.GetName() && sameKind(obj, tc.failGet) {
					return apierrors.NewInternalError(errors.New("the API server is struggling"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/network-data/"+networkDataTestNamespace+"/"+networkDataTestHost, nil)
	req.Header.Set("Authorization", "Bearer "+networkDataStatusToken)
	req.SetPathValue("namespace", networkDataTestNamespace)
	req.SetPathValue("hostName", networkDataTestHost)
	w := httptest.NewRecorder()
	(&NetworkDataHandler{Client: c, Log: logr.Discard()}).ServeHTTP(w, req)
	return w.Code
}

func sameKind(a, b client.Object) bool {
	switch b.(type) {
	case *infrav1.PhysicalHost:
		_, ok := a.(*infrav1.PhysicalHost)
		return ok
	case *corev1.Secret:
		_, ok := a.(*corev1.Secret)
		return ok
	}
	return false
}

// TestNetworkDataHandlerStatus pins what the inspector does with each answer:
// 404 installs without the file, so it is only "no ref"; a refusal is 401 and
// a failed read is 500, which the inspector retries.
func TestNetworkDataHandlerStatus(t *testing.T) {
	served := networkDataTestSecretFor(networkDataTestHost, networkDataTestBytes)
	noKey := networkDataTestSecretFor(networkDataTestHost, nil)
	noKey.Data = nil

	tests := []networkDataStatusCase{
		{name: "served", claimedBy: networkDataStatusMachine, secret: served, want: http.StatusOK},
		{name: "claim moved to another machine", claimedBy: "b7m-02", secret: served, want: http.StatusUnauthorized},
		{name: "host gone", claimedBy: networkDataStatusMachine, noHost: true, secret: served, want: http.StatusUnauthorized},
		{name: "host read fails", claimedBy: networkDataStatusMachine, secret: served,
			failGet: &infrav1.PhysicalHost{ObjectMeta: metav1.ObjectMeta{Name: networkDataTestHost}}, want: http.StatusInternalServerError},
		{name: "credentials read fails", claimedBy: networkDataStatusMachine, secret: served,
			failGet: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bootstrapTokenSecretName(networkDataTestHost)}}, want: http.StatusInternalServerError},
		{name: "network data read fails", claimedBy: networkDataStatusMachine, secret: served,
			failGet: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: networkDataTestSecret}}, want: http.StatusInternalServerError},
		{name: "no networkData key", claimedBy: networkDataStatusMachine, secret: noKey, want: http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serveNetworkDataStatus(t, tt); got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestClaimSkipsHostWithoutNetworkData proves a machine never claims a host
// whose network data cannot be served, and takes a usable host instead.
func TestClaimSkipsHostWithoutNetworkData(t *testing.T) {
	refused := func(name string) *infrav1.PhysicalHost {
		h := networkDataWaitHost(metav1.ConditionFalse, networkDataWaitGeneration)
		h.Name = name
		h.Status.State = infrav1.StateAvailable
		return h
	}
	usable := &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Namespace: networkDataTestNamespace, Name: "usable"},
		Status:     infrav1.PhysicalHostStatus{State: infrav1.StateAvailable},
	}

	tests := []struct {
		name      string
		hosts     []client.Object
		wantClaim string
	}{
		{name: "only a refused host", hosts: []client.Object{refused("refused")}},
		{name: "a refused and a usable host", hosts: []client.Object{refused("a-refused"), usable.DeepCopy()}, wantClaim: "usable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newSchemeForTest(t)
			machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Namespace: networkDataTestNamespace, Name: networkDataStatusMachine}}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(append(tt.hosts, machine)...).
				WithStatusSubresource(&infrav1.PhysicalHost{}).
				WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, func(o client.Object) []string {
					return []string{o.(*infrav1.PhysicalHost).Status.State}
				}).Build()
			r := &Beskar7MachineReconciler{Client: c, Scheme: scheme, Log: logr.Discard()}

			host, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), logr.Discard(), machine, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantClaim == "" {
				if host != nil {
					t.Fatalf("claimed %s, want no claim", host.Name)
				}
				return
			}
			if host == nil || host.Name != tt.wantClaim {
				t.Fatalf("claimed %v, want %s", host, tt.wantClaim)
			}
			for _, o := range tt.hosts {
				got := &infrav1.PhysicalHost{}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(o), got); err != nil {
					t.Fatal(err)
				}
				if claimed := got.Spec.ConsumerRef != nil; claimed != (got.Name == tt.wantClaim) {
					t.Fatalf("host %s claimed = %v", got.Name, claimed)
				}
			}
		})
	}
}
