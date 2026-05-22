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

package redfishmock

import (
	"net/http/httptest"
)

// MockRedfishServerWithHTTPTest wraps MockRedfishServer with an embedded
// httptest.Server so existing unit tests can use it without changes beyond
// swapping the constructor call.
type MockRedfishServerWithHTTPTest struct {
	*MockRedfishServer
	server *httptest.Server
}

// NewMockRedfishServerWithHTTPTest creates a MockRedfishServer backed by an
// httptest.NewTLSServer. Callers use GetURL() and Close() exactly as before.
func NewMockRedfishServerWithHTTPTest(vendor VendorType) *MockRedfishServerWithHTTPTest {
	mrs := NewMockRedfishServer(vendor)
	srv := httptest.NewTLSServer(mrs.Handler())
	return &MockRedfishServerWithHTTPTest{
		MockRedfishServer: mrs,
		server:            srv,
	}
}

// GetURL returns the httptest server URL (scheme://host).
func (m *MockRedfishServerWithHTTPTest) GetURL() string {
	return m.server.URL
}

// Close shuts down the underlying httptest server.
func (m *MockRedfishServerWithHTTPTest) Close() {
	m.server.Close()
}
