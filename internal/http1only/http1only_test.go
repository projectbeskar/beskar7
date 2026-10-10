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

package http1only

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"
)

func TestTLSConfigReplacesH2(t *testing.T) {
	// controller-runtime seeds the webhook and metrics TLS configs with h2
	// before it runs the TLSOpts.
	cfg := &tls.Config{NextProtos: []string{"h2"}}
	TLSConfig(cfg)
	if want := []string{"http/1.1"}; !slices.Equal(cfg.NextProtos, want) {
		t.Fatalf("NextProtos = %v, want %v", cfg.NextProtos, want)
	}
}

func TestServerSetsProtocolsAndALPN(t *testing.T) {
	s := &http.Server{}
	Server(s)

	if s.Protocols == nil || !s.Protocols.HTTP1() || s.Protocols.HTTP2() || s.Protocols.UnencryptedHTTP2() {
		t.Fatalf("Protocols = %v, want HTTP/1 only", s.Protocols)
	}
	if want := []string{"http/1.1"}; s.TLSConfig == nil || !slices.Equal(s.TLSConfig.NextProtos, want) {
		t.Fatalf("TLSConfig = %+v, want NextProtos %v", s.TLSConfig, want)
	}
}

// TestServerServesOnlyHTTP1 serves with ServeTLS, the path whose net/http
// default is to install HTTP/2, and offers h2 to it.
func TestServerServesOnlyHTTP1(t *testing.T) {
	cert := selfSignedCert(t)
	srv := &http.Server{
		// The h2-only client below makes the server log a refused handshake.
		ErrorLog: log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.Proto))
		}),
		TLSConfig: &tls.Config{
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil },
			MinVersion:     tls.VersionTLS12,
		},
	}
	Server(srv)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := l.Addr().String()

	dial := func(offer ...string) (string, error) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{
			InsecureSkipVerify: true, // self-signed; only the protocol is under test
			MinVersion:         tls.VersionTLS12,
			NextProtos:         offer,
		})
		if err != nil {
			return "", err
		}
		defer func() { _ = conn.Close() }()
		return conn.ConnectionState().NegotiatedProtocol, nil
	}

	if got, err := dial("h2", "http/1.1"); err != nil || got != "http/1.1" {
		t.Fatalf("offering h2 and http/1.1: negotiated %q, error %v; want http/1.1", got, err)
	}
	if got, err := dial("h2"); err == nil && got == "h2" {
		t.Fatal("offering only h2: negotiated h2")
	}

	// A client that would take h2 if it could still gets its answer.
	c := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		},
	}
	t.Cleanup(c.CloseIdleConnections)
	resp, err := c.Get("https://" + addr)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 1 {
		t.Fatalf("answered over %s, want HTTP/1.1", resp.Proto)
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "http1only-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
