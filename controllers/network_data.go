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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// networkDataSecretKey is the key holding the network data in the Secret
// spec.networkDataSecretRef names. Same key as Metal3's networkData Secret.
const networkDataSecretKey = "networkData"

// maxNetworkDataSize caps the served network data. A network-config v2 file
// with bonds, VLANs and routes is a few KiB.
const maxNetworkDataSize = 64 << 10

var (
	errNetworkDataSecretNotFound = errors.New("network data Secret not found")
	errNetworkDataNotAuthorized  = errors.New("network data Secret does not name this host in its " + NetworkDataHostAnnotation + " annotation")
	errNetworkDataInvalid        = errors.New("network data Secret has no usable " + networkDataSecretKey + " value")
)

// loadNetworkData returns the network data bytes for host. It returns nil and
// no error when the host has no spec.networkDataSecretRef. Every refusal wraps
// one of the errNetworkData* errors; any other error is a failed read.
// The Secret is read only in the host's own namespace.
func loadNetworkData(ctx context.Context, c client.Reader, host *infrav1.PhysicalHost) ([]byte, error) {
	name := host.Spec.NetworkDataSecretRef
	if name == "" {
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", errNetworkDataSecretNotFound, name)
		}
		return nil, fmt.Errorf("get network data Secret %s: %w", name, err)
	}
	if secret.Annotations[NetworkDataHostAnnotation] != host.Name {
		return nil, fmt.Errorf("%w: %s", errNetworkDataNotAuthorized, name)
	}
	data := secret.Data[networkDataSecretKey]
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: %s: key missing or empty", errNetworkDataInvalid, name)
	}
	if len(data) > maxNetworkDataSize {
		return nil, fmt.Errorf("%w: %s: exceeds %d bytes", errNetworkDataInvalid, name, maxNetworkDataSize)
	}
	return data, nil
}

// networkDataReason maps a loadNetworkData error to its NetworkDataReady
// reason, or "" for a failed read that says nothing about the Secret.
func networkDataReason(err error) string {
	switch {
	case errors.Is(err, errNetworkDataSecretNotFound):
		return infrav1.NetworkDataSecretNotFoundReason
	case errors.Is(err, errNetworkDataNotAuthorized):
		return infrav1.NetworkDataNotAuthorizedReason
	case errors.Is(err, errNetworkDataInvalid):
		return infrav1.NetworkDataInvalidReason
	}
	return ""
}
