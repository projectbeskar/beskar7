/*
Copyright 2026 The Beskar7 Authors.

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

package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestCallbackServerReloadsRenewedCertificate renews the callback server's
// certificate the way the kubelet updates a mounted Secret when cert-manager
// renews it, and checks that without a restart a new connection is served the
// renewed certificate and /boot hands out the renewed CA (SEC-14).
//
// Both halves matter. The default install issues the serving certificate from
// a selfSigned issuer, so every renewal changes the CA too: a server that
// reloads only its certificate while /boot keeps rendering the CA it read at
// startup breaks every host from the renewal on, earlier than one that
// reloads neither and breaks at expiry.
//
// The client dials an IP literal, as the inspector does, so it sends no SNI: a
// static certificate left in the server's tls.Config would win over the
// reloading one for exactly these clients.
func TestCallbackServerReloadsRenewedCertificate(t *testing.T) {
	restCfg := startEnvtest(t)
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	fx := createCallbackFixture(t, admin, createNamespace(t, admin, "cert-reload-"))

	certs := newSecretVolumeDir(t)
	first := certs.publish(t)
	base := runCallbackOnlyManager(t, restCfg, certs.dir)
	addr := strings.TrimPrefix(base, "https://")

	waitForServedLeaf(t, addr, first, 10*time.Second)
	if got := bootCA(t, fx.route(base, "boot"), first); !bytes.Equal(got, first.files["ca.crt"]) {
		t.Fatal("/boot does not hand out the CA of the certificate the server presents")
	}

	second := certs.publish(t)
	// The certwatcher reacts to the swap's filesystem events within
	// milliseconds and polls every 10s besides; 30s covers both.
	waitForServedLeaf(t, addr, second, 30*time.Second)

	// /boot re-serves a consumed nonce until it expires (§4.1), so the same URL
	// renders again. Poll: the CA follows the certificate, and /boot is
	// rate-limited to 1 r/s per client.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if got := bootCA(t, fx.route(base, "boot"), second); bytes.Equal(got, second.files["ca.crt"]) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("after renewal /boot still hands out the previous CA; hosts booting now would reject the renewed certificate")
		}
		time.Sleep(1100 * time.Millisecond)
	}
}

// secretVolumeDir lays out serving-certificate files the way the kubelet
// projects a Secret volume: tls.crt, tls.key and ca.crt are symlinks into
// ..data, itself a symlink to a per-generation directory. An update writes a
// new generation, swaps ..data with an atomic rename and removes the old
// generation.
type secretVolumeDir struct {
	dir        string
	generation int
}

func newSecretVolumeDir(t *testing.T) *secretVolumeDir {
	t.Helper()
	return &secretVolumeDir{dir: t.TempDir()}
}

func (d *secretVolumeDir) publish(t *testing.T) servingCert {
	t.Helper()
	cert := newServingCert(t)
	d.generation++
	gen := fmt.Sprintf("..gen-%d", d.generation)
	if err := os.Mkdir(filepath.Join(d.dir, gen), 0o700); err != nil {
		t.Fatalf("create generation dir: %v", err)
	}
	cert.writeTo(t, filepath.Join(d.dir, gen))

	tmp := filepath.Join(d.dir, "..data_tmp")
	if err := os.Symlink(gen, tmp); err != nil {
		t.Fatalf("link new generation: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(d.dir, "..data")); err != nil {
		t.Fatalf("swap ..data: %v", err)
	}
	if d.generation == 1 {
		for name := range cert.files {
			if err := os.Symlink(filepath.Join("..data", name), filepath.Join(d.dir, name)); err != nil {
				t.Fatalf("link %s: %v", name, err)
			}
		}
		return cert
	}
	if err := os.RemoveAll(filepath.Join(d.dir, fmt.Sprintf("..gen-%d", d.generation-1))); err != nil {
		t.Fatalf("remove previous generation: %v", err)
	}
	return cert
}

// waitForServedLeaf opens a fresh TLS connection to addr until the server
// presents want's leaf, failing after timeout.
func waitForServedLeaf(t *testing.T, addr string, want servingCert, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: want.pool(t), MinVersion: tls.VersionTLS12})
		if err == nil {
			served := conn.ConnectionState().PeerCertificates[0].Raw
			_ = conn.Close()
			if bytes.Equal(served, want.leaf) {
				return
			}
			last = "a different certificate signed by the same CA"
		} else {
			// Verification against want's CA fails while the previous
			// certificate is still served.
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("server at %s did not present the expected certificate within %s; last handshake: %s", addr, timeout, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

var bootCAParam = regexp.MustCompile(`beskar7\.ca=(\S+)`)

// bootCA fetches url over a connection that trusts trusted's CA and returns
// the PEM /boot rendered into beskar7.ca=, or nil when the request fails (the
// TLS handshake included). The script carries the host's bearer token, so it
// is never printed.
func bootCA(t *testing.T, url string, trusted servingCert) []byte {
	t.Helper()
	c := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: trusted.pool(t), MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
	}
	resp, err := c.Get(url)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var script bytes.Buffer
	if _, err := script.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read /boot response: %v", err)
	}
	m := bootCAParam.FindSubmatch(script.Bytes())
	if m == nil {
		t.Fatal("/boot response has no beskar7.ca= parameter")
	}
	ca, err := base64.StdEncoding.DecodeString(string(m[1]))
	if err != nil {
		t.Fatalf("beskar7.ca= is not base64: %v", err)
	}
	if pool := x509.NewCertPool(); !pool.AppendCertsFromPEM(ca) {
		t.Fatal("beskar7.ca= does not decode to a PEM certificate")
	}
	return ca
}
