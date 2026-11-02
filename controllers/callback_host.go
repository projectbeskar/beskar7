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
	"errors"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

const (
	headerContentType      = "Content-Type"
	headerCacheControl     = "Cache-Control"
	headerPragma           = "Pragma"
	contentTypeOctetStream = "application/octet-stream"
	cacheControlNoStore    = "no-store"
	pragmaNoCache          = "no-cache"
)

var errCallbackTokenMismatch = errors.New("bearer token does not match the host's current credentials")

// writeNoStore writes data as a 200 octet-stream that no proxy or HTTP cache
// may keep.
func writeNoStore(w http.ResponseWriter, data []byte) error {
	w.Header().Set(headerContentType, contentTypeOctetStream)
	w.Header().Set(headerCacheControl, cacheControlNoStore)
	w.Header().Set(headerPragma, pragmaNoCache)
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(data)
	return err
}

// boundCallbackHost resolves the PhysicalHost a claimed-host callback
// (/bootstrap, /networkdata) names and the Beskar7Machine its credentials are
// bound to, and checks the bearer token again against those credentials.
//
// The bearer verifier checked the token against its own read of the
// bootstrap-token Secret. A release and re-mint landing between that read and
// this one would pair the old token with the new claim, so the token is checked
// again here. The lookup is pinned to the host's own namespace, and only the
// machine the host's credentials were minted for is returned (SEC-12, D-029).
// Both endpoints go through this one function, so a change to who may call
// them changes both at once.
func boundCallbackHost(c client.Reader, r *http.Request) (*infrav1.PhysicalHost, types.NamespacedName, error) {
	namespace := r.PathValue("namespace")
	hostName := r.PathValue("hostName")
	if namespace == "" || hostName == "" {
		return nil, types.NamespacedName{}, errors.New("missing namespace or hostName in request path")
	}
	ph := &infrav1.PhysicalHost{}
	if err := c.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: hostName}, ph); err != nil {
		return nil, types.NamespacedName{}, fmt.Errorf("PhysicalHost lookup failed: %w", err)
	}
	creds, consumerKey, err := boundBootstrapCredentials(r.Context(), c, ph)
	if err != nil {
		return nil, types.NamespacedName{}, fmt.Errorf("PhysicalHost has no Beskar7Machine consumer bound to its credentials: %w", err)
	}
	token, ok := auth.BearerToken(r)
	if !ok || !creds.tokenValid(time.Now()) || !auth.Verify(token, auth.Hash(creds.token)) {
		return nil, types.NamespacedName{}, errCallbackTokenMismatch
	}
	return ph, consumerKey, nil
}
