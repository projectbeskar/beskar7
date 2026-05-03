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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// newSchemeForTest returns a scheme registered with the types we need so the
// fake client can serve PhysicalHost + Secret objects.
func newSchemeForTest(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := infrastructurev1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1beta1 to scheme: %v", err)
	}
	return scheme
}

func TestFetchRedfishCABundle_NoRef(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(newSchemeForTest(t)).Build()
	host := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns"},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			RedfishConnection: infrastructurev1beta1.RedfishConnection{
				Address:              "https://bmc.example.com",
				CredentialsSecretRef: "creds",
			},
		},
	}
	bundle, err := fetchRedfishCABundle(context.Background(), c, host)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bundle != nil {
		t.Fatalf("expected nil bundle when CABundleSecretRef is unset, got %d bytes", len(bundle))
	}
}

func TestFetchRedfishCABundle_PrefersCACrt(t *testing.T) {
	t.Parallel()
	scheme := newSchemeForTest(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ca-bundle", Namespace: "ns"},
		Data: map[string][]byte{
			"ca.crt":  []byte("CA-CRT-PEM-BYTES"),
			"tls.crt": []byte("TLS-CRT-PEM-BYTES"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	host := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns"},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			RedfishConnection: infrastructurev1beta1.RedfishConnection{
				Address:              "https://bmc.example.com",
				CredentialsSecretRef: "creds",
				CABundleSecretRef:    &corev1.LocalObjectReference{Name: "ca-bundle"},
			},
		},
	}
	bundle, err := fetchRedfishCABundle(context.Background(), c, host)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(bundle) != "CA-CRT-PEM-BYTES" {
		t.Fatalf("expected ca.crt to win when both keys present, got %q", string(bundle))
	}
}

func TestFetchRedfishCABundle_FallsBackToTLSCrt(t *testing.T) {
	t.Parallel()
	scheme := newSchemeForTest(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ca-bundle", Namespace: "ns"},
		Data: map[string][]byte{
			"tls.crt": []byte("TLS-CRT-PEM-BYTES"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	host := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns"},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			RedfishConnection: infrastructurev1beta1.RedfishConnection{
				Address:              "https://bmc.example.com",
				CredentialsSecretRef: "creds",
				CABundleSecretRef:    &corev1.LocalObjectReference{Name: "ca-bundle"},
			},
		},
	}
	bundle, err := fetchRedfishCABundle(context.Background(), c, host)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(bundle) != "TLS-CRT-PEM-BYTES" {
		t.Fatalf("expected tls.crt fallback, got %q", string(bundle))
	}
}

func TestFetchRedfishCABundle_SecretMissing(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(newSchemeForTest(t)).Build()
	host := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns"},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			RedfishConnection: infrastructurev1beta1.RedfishConnection{
				Address:              "https://bmc.example.com",
				CredentialsSecretRef: "creds",
				CABundleSecretRef:    &corev1.LocalObjectReference{Name: "missing"},
			},
		},
	}
	_, err := fetchRedfishCABundle(context.Background(), c, host)
	if err == nil {
		t.Fatal("expected error for missing CA bundle secret, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestFetchRedfishCABundle_NoUsableKeys(t *testing.T) {
	t.Parallel()
	scheme := newSchemeForTest(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ca-bundle", Namespace: "ns"},
		Data: map[string][]byte{
			// Wrong keys — neither ca.crt nor tls.crt.
			"bundle.pem": []byte("PEM-BYTES"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	host := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns"},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			RedfishConnection: infrastructurev1beta1.RedfishConnection{
				Address:              "https://bmc.example.com",
				CredentialsSecretRef: "creds",
				CABundleSecretRef:    &corev1.LocalObjectReference{Name: "ca-bundle"},
			},
		},
	}
	_, err := fetchRedfishCABundle(context.Background(), c, host)
	if err == nil {
		t.Fatal("expected error for secret with no usable keys, got nil")
	}
	if !strings.Contains(err.Error(), "no usable") {
		t.Fatalf("expected 'no usable' error, got: %v", err)
	}
}

// TestFetchRedfishCABundle_EmptyDataValue verifies that a present-but-empty
// data key is treated as missing (otherwise we'd silently pass empty bytes
// to the factory, which would then fall through to system roots — exactly
// the operator-intent-defeating behaviour we want to surface).
func TestFetchRedfishCABundle_EmptyDataValue(t *testing.T) {
	t.Parallel()
	scheme := newSchemeForTest(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ca-bundle", Namespace: "ns"},
		Data: map[string][]byte{
			"ca.crt":  {},
			"tls.crt": {},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	host := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns"},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			RedfishConnection: infrastructurev1beta1.RedfishConnection{
				Address:              "https://bmc.example.com",
				CredentialsSecretRef: "creds",
				CABundleSecretRef:    &corev1.LocalObjectReference{Name: "ca-bundle"},
			},
		},
	}
	_, err := fetchRedfishCABundle(context.Background(), c, host)
	if err == nil {
		t.Fatal("expected error when both keys are empty, got nil")
	}
}

func TestValidateRedfishTLSCombination(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		insecure  bool
		bundleRef *corev1.LocalObjectReference
		wantErr   bool
	}{
		{name: "neither", insecure: false, bundleRef: nil, wantErr: false},
		{name: "insecure-only", insecure: true, bundleRef: nil, wantErr: false},
		{name: "bundle-only", insecure: false, bundleRef: &corev1.LocalObjectReference{Name: "b"}, wantErr: false},
		{name: "both-rejected", insecure: true, bundleRef: &corev1.LocalObjectReference{Name: "b"}, wantErr: true},
		// Empty-name LocalObjectReference is treated as "no ref" — defensive
		// behaviour for callers who construct an empty reference object.
		{name: "insecure-and-empty-ref", insecure: true, bundleRef: &corev1.LocalObjectReference{Name: ""}, wantErr: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRedfishTLSCombination(tc.insecure, tc.bundleRef)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}
