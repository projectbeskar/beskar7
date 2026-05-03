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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// CA bundle Secret data keys, in lookup-precedence order: "ca.crt" wins if both
// are present. "ca.crt" is the kubernetes/cert-manager convention for CA-only
// bundles; "tls.crt" is the kubernetes.io/tls Secret convention and may be a
// server cert chain — accepting it is a convenience but ca.crt is preferred.
const (
	caBundleKeyCA  = "ca.crt"
	caBundleKeyTLS = "tls.crt"
)

// fetchRedfishCABundle returns PEM bytes from the BMC CA bundle Secret
// referenced by host.Spec.RedfishConnection.CABundleSecretRef, or nil if no
// such ref is set. The Secret must live in the same namespace as the host.
//
// Precedence: data["ca.crt"] is preferred; if absent, data["tls.crt"] is used.
// If neither is present (or both are empty), returns an error so the caller
// can mark a clear condition rather than silently fall through to system roots.
//
// The Secret data is non-sensitive (a public CA bundle), but we still avoid
// logging it; the only diagnostic we emit is the byte length, via the caller.
func fetchRedfishCABundle(ctx context.Context, c client.Reader, host *infrastructurev1beta1.PhysicalHost) ([]byte, error) {
	ref := host.Spec.RedfishConnection.CABundleSecretRef
	if ref == nil || ref.Name == "" {
		return nil, nil
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: host.Namespace, Name: ref.Name}
	if err := c.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("CA bundle secret %q not found in namespace %q", ref.Name, host.Namespace)
		}
		return nil, fmt.Errorf("failed to get CA bundle secret %q: %w", ref.Name, err)
	}

	if data, ok := secret.Data[caBundleKeyCA]; ok && len(data) > 0 {
		return data, nil
	}
	if data, ok := secret.Data[caBundleKeyTLS]; ok && len(data) > 0 {
		return data, nil
	}
	return nil, fmt.Errorf("CA bundle secret %q has no usable %q or %q data key",
		ref.Name, caBundleKeyCA, caBundleKeyTLS)
}

// validateRedfishTLSCombination rejects the (InsecureSkipVerify=true,
// CABundleSecretRef!=nil) combination. The two are mutually exclusive: a custom
// CA bundle and "skip verification" together is incoherent and almost certainly
// an operator misconfiguration. Returns nil when the configuration is valid.
func validateRedfishTLSCombination(insecure bool, caBundleRef *corev1.LocalObjectReference) error {
	if insecure && caBundleRef != nil && caBundleRef.Name != "" {
		return fmt.Errorf(
			"redfishConnection.insecureSkipVerify=true is mutually exclusive with caBundleSecretRef; choose one")
	}
	return nil
}
