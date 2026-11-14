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
	"bytes"
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const (
	networkDataTestNamespace = "netdata"
	networkDataTestHost      = "paris"
	networkDataTestSecret    = "paris-net"

	networkDataTestConsumerUID = types.UID("uid-machine-b")
	networkDataTestOldUID      = "uid-machine-a"
)

var networkDataTestBytes = []byte("network:\n  version: 2\n")

func networkDataTestHostWithRef(ref string) *infrav1.PhysicalHost {
	return &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Namespace: networkDataTestNamespace, Name: networkDataTestHost},
		Spec:       infrav1.PhysicalHostSpec{NetworkDataSecretRef: ref},
	}
}

func networkDataTestSecretFor(annotatedHost string, data []byte) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: networkDataTestNamespace, Name: networkDataTestSecret},
		Data:       map[string][]byte{networkDataSecretKey: data},
	}
	if annotatedHost != "" {
		secret.Annotations = map[string]string{NetworkDataHostAnnotation: annotatedHost}
	}
	return secret
}

func TestLoadNetworkData(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		secret     *corev1.Secret
		wantData   []byte
		wantReason string
	}{
		{name: "no ref", ref: ""},
		{name: "served", ref: networkDataTestSecret, secret: networkDataTestSecretFor(networkDataTestHost, networkDataTestBytes), wantData: networkDataTestBytes},
		{name: "missing Secret", ref: networkDataTestSecret, wantReason: infrav1.NetworkDataSecretNotFoundReason},
		{name: "no annotation", ref: networkDataTestSecret, secret: networkDataTestSecretFor("", networkDataTestBytes), wantReason: infrav1.NetworkDataNotAuthorizedReason},
		{name: "annotation names another host", ref: networkDataTestSecret, secret: networkDataTestSecretFor("milan", networkDataTestBytes), wantReason: infrav1.NetworkDataNotAuthorizedReason},
		{name: "empty value", ref: networkDataTestSecret, secret: networkDataTestSecretFor(networkDataTestHost, nil), wantReason: infrav1.NetworkDataInvalidReason},
		{name: "over the cap", ref: networkDataTestSecret, secret: networkDataTestSecretFor(networkDataTestHost, bytes.Repeat([]byte("a"), maxNetworkDataSize+1)), wantReason: infrav1.NetworkDataInvalidReason},
		{name: "at the cap", ref: networkDataTestSecret, secret: networkDataTestSecretFor(networkDataTestHost, bytes.Repeat([]byte("a"), maxNetworkDataSize)), wantData: bytes.Repeat([]byte("a"), maxNetworkDataSize)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.secret != nil {
				objects = append(objects, tt.secret)
			}
			c := fake.NewClientBuilder().WithScheme(newSchemeForTest(t)).WithObjects(objects...).Build()

			data, err := loadNetworkData(context.Background(), c, networkDataTestHostWithRef(tt.ref))

			if got := networkDataReason(err); got != tt.wantReason {
				t.Fatalf("reason = %q, want %q (err: %v)", got, tt.wantReason, err)
			}
			if tt.wantReason == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(data, tt.wantData) {
				t.Fatalf("data = %q, want %q", data, tt.wantData)
			}
		})
	}
}

func networkDataTestSecretForConsumer(consumer string) *corev1.Secret {
	secret := networkDataTestSecretFor(networkDataTestHost, networkDataTestBytes)
	secret.Annotations[NetworkDataConsumerAnnotation] = consumer
	return secret
}

func networkDataTestClaimedHost() *infrav1.PhysicalHost {
	host := networkDataTestHostWithRef(networkDataTestSecret)
	host.Spec.ConsumerRef = &corev1.ObjectReference{UID: networkDataTestConsumerUID}
	return host
}

func TestLoadNetworkDataConsumer(t *testing.T) {
	tests := []struct {
		name       string
		host       *infrav1.PhysicalHost
		secret     *corev1.Secret
		wantReason string
	}{
		{name: "claimed, no consumer annotation", host: networkDataTestClaimedHost(), secret: networkDataTestSecretFor(networkDataTestHost, networkDataTestBytes)},
		{name: "claimed, consumer matches", host: networkDataTestClaimedHost(), secret: networkDataTestSecretForConsumer(string(networkDataTestConsumerUID))},
		{name: "claimed, consumer is the previous claim", host: networkDataTestClaimedHost(), secret: networkDataTestSecretForConsumer(networkDataTestOldUID), wantReason: infrav1.NetworkDataStaleReason},
		{name: "claimed, consumer annotation empty", host: networkDataTestClaimedHost(), secret: networkDataTestSecretForConsumer(""), wantReason: infrav1.NetworkDataStaleReason},
		{name: "unclaimed, consumer is the previous claim", host: networkDataTestHostWithRef(networkDataTestSecret), secret: networkDataTestSecretForConsumer(networkDataTestOldUID)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newSchemeForTest(t)).WithObjects(tt.secret).Build()

			data, err := loadNetworkData(context.Background(), c, tt.host)

			if got := networkDataReason(err); got != tt.wantReason {
				t.Fatalf("reason = %q, want %q (err: %v)", got, tt.wantReason, err)
			}
			if tt.wantReason != "" {
				if data != nil {
					t.Fatalf("data = %q, want none", data)
				}
				return
			}
			if err != nil || !bytes.Equal(data, networkDataTestBytes) {
				t.Fatalf("data = %q, err = %v, want %q", data, err, networkDataTestBytes)
			}
		})
	}
}

func TestReconcileNetworkDataCondition(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		secret     *corev1.Secret
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{name: "no ref removes the condition", ref: ""},
		{name: "served", ref: networkDataTestSecret, secret: networkDataTestSecretFor(networkDataTestHost, networkDataTestBytes), wantStatus: metav1.ConditionTrue, wantReason: infrav1.NetworkDataAvailableReason},
		{name: "missing Secret", ref: networkDataTestSecret, wantStatus: metav1.ConditionFalse, wantReason: infrav1.NetworkDataSecretNotFoundReason},
		{name: "not opted in", ref: networkDataTestSecret, secret: networkDataTestSecretFor("milan", networkDataTestBytes), wantStatus: metav1.ConditionFalse, wantReason: infrav1.NetworkDataNotAuthorizedReason},
		{name: "invalid", ref: networkDataTestSecret, secret: networkDataTestSecretFor(networkDataTestHost, nil), wantStatus: metav1.ConditionFalse, wantReason: infrav1.NetworkDataInvalidReason},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.secret != nil {
				objects = append(objects, tt.secret)
			}
			c := fake.NewClientBuilder().WithScheme(newSchemeForTest(t)).WithObjects(objects...).Build()
			r := &PhysicalHostReconciler{Client: c}
			host := networkDataTestHostWithRef(tt.ref)
			// A stale condition from an earlier ref must not survive.
			setTrue(host, infrav1.NetworkDataReadyCondition, infrav1.NetworkDataAvailableReason)

			r.reconcileNetworkData(context.Background(), logr.Discard(), host)

			cond := conditions.Get(host, infrav1.NetworkDataReadyCondition)
			if tt.wantReason == "" {
				if cond != nil {
					t.Fatalf("condition = %+v, want none", cond)
				}
				return
			}
			if cond == nil {
				t.Fatal("condition missing")
			}
			if cond.Status != tt.wantStatus || cond.Reason != tt.wantReason {
				t.Fatalf("condition = %s/%s, want %s/%s", cond.Status, cond.Reason, tt.wantStatus, tt.wantReason)
			}
		})
	}
}

func TestSecretToPhysicalHostsMapsNetworkDataSecret(t *testing.T) {
	host := networkDataTestHostWithRef(networkDataTestSecret)
	other := networkDataTestHostWithRef("")
	other.Name = "milan"
	c := fake.NewClientBuilder().WithScheme(newSchemeForTest(t)).WithObjects(host, other).Build()
	r := &PhysicalHostReconciler{Client: c}

	got := r.SecretToPhysicalHosts(context.Background(), networkDataTestSecretFor(networkDataTestHost, networkDataTestBytes))

	if len(got) != 1 || got[0].Name != networkDataTestHost || got[0].Namespace != networkDataTestNamespace {
		t.Fatalf("requests = %v, want only %s/%s", got, networkDataTestNamespace, networkDataTestHost)
	}
}
