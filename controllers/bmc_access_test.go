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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

func TestBMCAllowListMatching(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		list string
		host string
		want bool
	}{
		{"wildcard matches one label under the suffix", "*.b.c", "a.b.c", true},
		{"wildcard matches several labels under the suffix", "*.b.c", "x.y.b.c", true},
		{"wildcard does not match the bare suffix", "*.b.c", "b.c", false},
		{"wildcard does not match an empty label before the suffix", "*.b.c", ".b.c", false},
		{"wildcard does not match a longer label ending in the suffix", "*.b.c", "xb.c", false},
		{"wildcard does not match a different suffix", "*.b.c", "a.b.cx", false},
		{"wildcard does not match an IP literal ending in the suffix", "*.0.0.5", "10.0.0.5", false},
		{"hostname matches exactly", "bmc.example.com", "bmc.example.com", true},
		{"hostname does not match a subdomain", "bmc.example.com", "a.bmc.example.com", false},
		{"hostname entry is case-insensitive", "BMC.Example.COM", "bmc.example.com", true},
		{"hostname address is case-insensitive", "bmc.example.com", "BMC.EXAMPLE.com", true},
		{"wildcard is case-insensitive", "*.Example.com", "A.EXAMPLE.COM", true},
		{"IP matches exactly", "10.0.0.5", "10.0.0.5", true},
		{"IP does not match a neighbour", "10.0.0.5", "10.0.0.6", false},
		{"CIDR matches an IP inside it", "10.0.0.0/24", "10.0.0.200", true},
		{"CIDR does not match an IP outside it", "10.0.0.0/24", "10.0.1.5", false},
		{"CIDR with host bits set matches its network", "10.0.0.9/24", "10.0.0.200", true},
		{"CIDR never matches a hostname that starts with an IP inside it", "10.0.0.0/24", "10.0.0.5.nip.io", false},
		{"CIDR never matches a hostname", "10.0.0.0/8", "bmc.example.com", false},
		{"IPv6 literal matches", "2001:db8::1", "2001:db8::1", true},
		{"IPv6 literal matches another spelling of the same address", "2001:db8::1", "2001:0db8:0:0:0:0:0:1", true},
		{"IPv6 literal does not match a neighbour", "2001:db8::1", "2001:db8::2", false},
		{"IPv6 CIDR matches inside", "2001:db8::/32", "2001:db8:1::5", true},
		{"IPv6 CIDR does not match outside", "2001:db8::/32", "2001:db9::1", false},
		{"IPv6 CIDR does not match an IPv4 address", "::/32", "10.0.0.5", false},
		{"IPv4 CIDR does not match an IPv6 address", "0.0.0.0/8", "2001:db8::1", false},
		{"an IPv4-mapped IPv6 address is the IPv4 address", "10.0.0.0/24", "::ffff:10.0.0.5", true},
		{"the widest IPv4 CIDR accepted is a /8", "10.0.0.0/8", "10.200.0.1", true},
		{"a wildcard under a private name below a public suffix", "*.bmc.lab", "r1.bmc.lab", true},
		{"a wildcard under a cluster-local domain", "*.beskar7-smoke.svc", "mock-redfish.beskar7-smoke.svc", true},
		{"entries may be separated by commas, spaces and newlines", "a.example.com,b.example.com  c.example.com\n\t10.0.0.0/8", "c.example.com", true},
		{"every entry of a list is tried", "a.example.com, 10.0.0.0/8", "10.1.2.3", true},
		{"no entry matches", "a.example.com, 10.0.0.0/8", "192.168.1.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			list, err := parseBMCAllowList(tc.list)
			if err != nil {
				t.Fatalf("parseBMCAllowList(%q): %v", tc.list, err)
			}
			if got := list.allows(tc.host); got != tc.want {
				t.Fatalf("list %q allows(%q) = %v, want %v", tc.list, tc.host, got, tc.want)
			}
		})
	}
}

func TestBMCAccessNeverFormatsCredentials(t *testing.T) {
	t.Parallel()
	a := bmcAccess{username: "bmc-admin-user", password: "hunter2-bmc-password"}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(verb, a); strings.Contains(out, a.username) || strings.Contains(out, a.password) {
			t.Fatalf("formatting a bmcAccess with %s printed its credentials: %s", verb, out)
		}
	}
}

func TestBMCAllowListRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, list := range []string{
		"",
		" ,, \n",
		"10.0.0.0/33",
		"10.0.0.0/",
		"/24",
		"bmc.example.com/24",
		"*",
		"*.",
		"**.example.com",
		"a.*.example.com",
		"*example.com",
		"bmc_1.example.com",
		"-bmc.example.com",
		"bmc.example.com.",
		"..example.com",
		"https://bmc.example.com",
		"10.0.0.5:443",
		"[2001:db8::1]",
		"fe80::1%eth0",
		"bmc.exämple.com",
		// Catch-alls: a CIDR wider than /8 (IPv4) or /32 (IPv6), or a wildcard
		// whose suffix is itself a public suffix, would send the credentials to
		// anyone who can register a name or run a pod there.
		"0.0.0.0/0",
		"10.0.0.0/7",
		"::/0",
		"2001::/31",
		"*.com",
		"*.co.uk",
		"*.lab",
		"*.svc",
		"*.local",
		"*.github.io",
		// An IPv4-mapped CIDR would never match: allows compares the unmapped
		// address, so accepting it would silently narrow the list.
		"::ffff:10.0.0.0/104",
		// One malformed entry rejects the whole annotation, not just itself.
		"10.0.0.0/24, bmc_1.example.com",
		"bmc.example.com host!",
	} {
		t.Run(list, func(t *testing.T) {
			t.Parallel()
			got, err := parseBMCAllowList(list)
			if err == nil {
				t.Fatalf("parseBMCAllowList(%q) accepted a malformed list: %+v", list, got)
			}
			if got.allows("10.0.0.5") || got.allows("bmc.example.com") {
				t.Fatalf("a rejected list must authorise nothing")
			}
		})
	}
}

func TestParseBMCAddress(t *testing.T) {
	t.Parallel()
	valid := []struct {
		address   string
		host      string
		plaintext bool
	}{
		{"https://10.0.0.5", "10.0.0.5", false},
		{"https://10.0.0.5:8443/redfish/v1", "10.0.0.5", false},
		{"http://bmc.example.com:8000", "bmc.example.com", true},
		{"https://BMC.Example.com", "bmc.example.com", false},
		{"https://[2001:db8::1]:443", "2001:db8::1", false},
	}
	for _, tc := range valid {
		host, plaintext, err := parseBMCAddress(tc.address)
		if err != nil {
			t.Errorf("parseBMCAddress(%q): %v", tc.address, err)
			continue
		}
		if host != tc.host || plaintext != tc.plaintext {
			t.Errorf("parseBMCAddress(%q) = (%q, %v), want (%q, %v)", tc.address, host, plaintext, tc.host, tc.plaintext)
		}
	}

	for _, address := range []string{
		"https://admin:hunter2@10.0.0.5",
		"https://admin@10.0.0.5",
		"ftp://10.0.0.5",
		"redfish://10.0.0.5",
		"10.0.0.5",
		"10.0.0.5:443",
		"bmc.example.com",
		"https://",
		"https:///redfish/v1",
		"https:10.0.0.5",
		"https://bmc_1.example.com",
		"https://..example.com",
		"https://[fe80::1%25eth0]",
		"https://10.0.0.5:notaport",
	} {
		host, _, err := parseBMCAddress(address)
		if err == nil {
			t.Errorf("parseBMCAddress(%q) accepted it with host %q", address, host)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("parseBMCAddress(%q) put the address's password in its error: %v", address, err)
		}
	}
}

// resolveBMCAccess is the single gate both controllers pass through. Every
// failure maps to the RedfishConnectionReady reason the host publishes, and
// none of them carries the credential.
func TestResolveBMCAccess(t *testing.T) {
	t.Parallel()
	const (
		ns       = "ns"
		username = "gate-user"
		password = "gate-password"
		bundle   = "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"
	)
	credentials := func(annotations map[string]string, data map[string][]byte) *corev1.Secret {
		if data == nil {
			data = map[string][]byte{"username": []byte(username), "password": []byte(password)}
		}
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns, Annotations: annotations}, Data: data}
	}
	listed := func(list string) map[string]string {
		return map[string]string{BMCAddressesAnnotation: list}
	}
	listedInsecure := func(list, optIn string) map[string]string {
		return map[string]string{BMCAddressesAnnotation: list, BMCInsecureTransportAnnotation: optIn}
	}
	caBundle := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-ca", Namespace: ns},
		Data:       map[string][]byte{"ca.crt": []byte(bundle)},
	}

	cases := []struct {
		name       string
		connection infrav1.RedfishConnection
		objects    []client.Object
		wantReason string
		want       bmcAccess
	}{
		{
			name:       "an https address on the list",
			connection: infrav1.RedfishConnection{Address: "https://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed("10.0.0.0/24"), nil)},
			want:       bmcAccess{username: username, password: password},
		},
		{
			name: "an https address on the list with a CA bundle",
			connection: infrav1.RedfishConnection{
				Address: "https://bmc.example.com", CredentialsSecretRef: "creds", CABundleSecretRef: "bmc-ca",
			},
			objects: []client.Object{credentials(listed("*.example.com"), nil), caBundle},
			want:    bmcAccess{username: username, password: password, caBundle: []byte(bundle)},
		},
		{
			name: "insecureSkipVerify with the opt-in",
			connection: infrav1.RedfishConnection{
				Address: "https://10.0.0.5", CredentialsSecretRef: "creds", InsecureSkipVerify: ptr.To(true),
			},
			objects: []client.Object{credentials(listedInsecure("10.0.0.5", "true"), nil)},
			want:    bmcAccess{username: username, password: password, insecure: true},
		},
		{
			name:       "http with the opt-in",
			connection: infrav1.RedfishConnection{Address: "http://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listedInsecure("10.0.0.5", "true"), nil)},
			want:       bmcAccess{username: username, password: password},
		},
		{
			name:       "no credentialsSecretRef",
			connection: infrav1.RedfishConnection{Address: "https://10.0.0.5"},
			wantReason: infrav1.MissingCredentialsReason,
		},
		{
			name:       "the credentials Secret does not exist",
			connection: infrav1.RedfishConnection{Address: "https://10.0.0.5", CredentialsSecretRef: "creds"},
			wantReason: infrav1.MissingCredentialsReason,
		},
		{
			name:       "no bmc-addresses annotation",
			connection: infrav1.RedfishConnection{Address: "https://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(nil, nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name:       "an empty bmc-addresses annotation",
			connection: infrav1.RedfishConnection{Address: "https://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed(" "), nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name:       "a malformed entry next to one that would match",
			connection: infrav1.RedfishConnection{Address: "https://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed("10.0.0.0/24, not_a_host"), nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name:       "an address that is not on the list",
			connection: infrav1.RedfishConnection{Address: "https://203.0.113.9", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed("10.0.0.0/24"), nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name:       "http without the opt-in",
			connection: infrav1.RedfishConnection{Address: "http://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed("10.0.0.5"), nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name: "insecureSkipVerify without the opt-in",
			connection: infrav1.RedfishConnection{
				Address: "https://10.0.0.5", CredentialsSecretRef: "creds", InsecureSkipVerify: ptr.To(true),
			},
			objects:    []client.Object{credentials(listed("10.0.0.5"), nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name: "an opt-in that is not exactly \"true\"",
			connection: infrav1.RedfishConnection{
				Address: "https://10.0.0.5", CredentialsSecretRef: "creds", InsecureSkipVerify: ptr.To(true),
			},
			objects:    []client.Object{credentials(listedInsecure("10.0.0.5", "True"), nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name:       "an address with userinfo",
			connection: infrav1.RedfishConnection{Address: "https://admin:hunter2@10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed("10.0.0.5"), nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name:       "an address that is not http(s)",
			connection: infrav1.RedfishConnection{Address: "ftp://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed("10.0.0.5"), nil)},
			wantReason: infrav1.CredentialsNotAuthorizedReason,
		},
		{
			name: "insecureSkipVerify with a CA bundle, opted in",
			connection: infrav1.RedfishConnection{
				Address: "https://10.0.0.5", CredentialsSecretRef: "creds",
				InsecureSkipVerify: ptr.To(true), CABundleSecretRef: "bmc-ca",
			},
			objects:    []client.Object{credentials(listedInsecure("10.0.0.5", "true"), nil), caBundle},
			wantReason: infrav1.InsecureCABundleConflictReason,
		},
		{
			name:       "no password key",
			connection: infrav1.RedfishConnection{Address: "https://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed("10.0.0.5"), map[string][]byte{"username": []byte(username)})},
			wantReason: infrav1.MissingCredentialsReason,
		},
		{
			name:       "no username key",
			connection: infrav1.RedfishConnection{Address: "https://10.0.0.5", CredentialsSecretRef: "creds"},
			objects:    []client.Object{credentials(listed("10.0.0.5"), map[string][]byte{"password": []byte(password)})},
			wantReason: infrav1.MissingCredentialsReason,
		},
		{
			name: "the CA bundle Secret does not exist",
			connection: infrav1.RedfishConnection{
				Address: "https://10.0.0.5", CredentialsSecretRef: "creds", CABundleSecretRef: "bmc-ca",
			},
			objects:    []client.Object{credentials(listed("10.0.0.5"), nil)},
			wantReason: infrav1.CABundleFetchFailedReason,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The builder stamps a resourceVersion on what it is given, and the
			// subtests run in parallel over shared fixtures.
			objects := make([]client.Object, 0, len(tc.objects))
			for _, o := range tc.objects {
				objects = append(objects, o.DeepCopyObject().(client.Object))
			}
			c := fake.NewClientBuilder().WithScheme(newSchemeForTest(t)).WithObjects(objects...).Build()
			host := &infrav1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: ns},
				Spec:       infrav1.PhysicalHostSpec{RedfishConnection: tc.connection},
			}
			got, err := resolveBMCAccess(context.Background(), c, host)
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("resolveBMCAccess: %v", err)
				}
				if got.username != tc.want.username || got.password != tc.want.password ||
					got.insecure != tc.want.insecure || !bytes.Equal(got.caBundle, tc.want.caBundle) {
					t.Fatalf("resolveBMCAccess = %+v, want %+v", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveBMCAccess authorised it: %+v", got)
			}
			if reason := bmcAccessReason(err); reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q (err: %v)", reason, tc.wantReason, err)
			}
			if got.password != "" || got.username != "" {
				t.Fatalf("a refusal must not hand back the credentials")
			}
			for _, leaked := range []string{password, username, "hunter2"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("error %q leaks %q", err, leaked)
				}
			}
		})
	}
}
