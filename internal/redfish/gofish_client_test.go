package redfish

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// generateSelfSignedCA returns a PEM bundle and an *http.Client whose
// transport trusts that bundle, plus a *tls.Config the test server can serve.
// Used by the CA-bundle tests to verify newHTTPClient and NewClient honour
// the supplied bundle without falling back to system roots.
func generateSelfSignedCA(t *testing.T) (caPEM []byte, serverCert tls.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "beskar7-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"127.0.0.1", "localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	serverCert = tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}
	return caPEM, serverCert
}

// ---------------------------------------------------------------------------
// doWithCtx
// ---------------------------------------------------------------------------

func TestDoWithCtx_Success(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	err := doWithCtx(ctx, func() error { return nil })
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestDoWithCtx_OpError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	want := errors.New("op failed")
	err := doWithCtx(ctx, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
}

func TestDoWithCtx_CancelledBeforeOp(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	// The op blocks for 500 ms; we cancel after 50 ms.
	err := doWithCtx(ctx, func() error {
		cancel()
		// Simulate work that takes longer than the cancellation.
		time.Sleep(500 * time.Millisecond)
		return nil
	})
	// We cancelled ctx inside the op; doWithCtx should return ctx.Err().
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDoWithCtx_DeadlineExceeded(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := doWithCtx(ctx, func() error {
		time.Sleep(500 * time.Millisecond)
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// newHTTPClient
// ---------------------------------------------------------------------------

func TestNewHTTPClient_Timeout(t *testing.T) {
	t.Parallel()
	c, err := newHTTPClient(false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Timeout != defaultHTTPTimeout {
		t.Fatalf("expected Timeout=%v, got %v", defaultHTTPTimeout, c.Timeout)
	}
}

func TestNewHTTPClient_InsecureFalse(t *testing.T) {
	t.Parallel()
	c, err := newHTTPClient(false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=false")
	}
	if transport.TLSClientConfig.RootCAs != nil {
		t.Fatal("expected RootCAs=nil (system roots) when no CA bundle supplied")
	}
}

func TestNewHTTPClient_InsecureTrue(t *testing.T) {
	t.Parallel()
	c, err := newHTTPClient(true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=true")
	}
}

// TestNewHTTPClient_CABundle_Valid verifies that a valid PEM bundle results in
// a Transport whose RootCAs pool is populated AND InsecureSkipVerify is forced
// to false (defence in depth — caller should already have rejected the
// (insecure=true, caBundle!=nil) combination at the factory layer).
func TestNewHTTPClient_CABundle_Valid(t *testing.T) {
	t.Parallel()
	caPEM, _ := generateSelfSignedCA(t)
	c, err := newHTTPClient(false, caPEM)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("expected RootCAs to be populated when CA bundle supplied")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=false when CA bundle supplied")
	}
}

// TestNewHTTPClient_CABundle_InsecureForcedFalse verifies the defence-in-depth
// behaviour: even if a caller bypasses the factory check and supplies both
// insecure=true and a CA bundle, newHTTPClient still forces InsecureSkipVerify
// to false. The factory layer is the canonical reject; this is the second gate.
func TestNewHTTPClient_CABundle_InsecureForcedFalse(t *testing.T) {
	t.Parallel()
	caPEM, _ := generateSelfSignedCA(t)
	c, err := newHTTPClient(true, caPEM)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=false even when insecure=true was passed alongside a CA bundle")
	}
}

// TestNewHTTPClient_CABundle_MalformedPEM verifies that bytes that don't
// parse as PEM produce an explicit error (silent fallback to system roots
// would defeat the operator's intent).
func TestNewHTTPClient_CABundle_MalformedPEM(t *testing.T) {
	t.Parallel()
	bogus := []byte("this is not a PEM-encoded certificate")
	_, err := newHTTPClient(false, bogus)
	if err == nil {
		t.Fatal("expected error for malformed PEM, got nil")
	}
}

// TestNewClient_RejectsInsecureWithCABundle verifies the factory rejects the
// mutually exclusive combination loudly. This is the canonical guard; the
// transport-level guard is defence in depth.
func TestNewClient_RejectsInsecureWithCABundle(t *testing.T) {
	t.Parallel()
	caPEM, _ := generateSelfSignedCA(t)
	_, err := NewClient(context.Background(), "https://127.0.0.1:1", "u", "p", true, caPEM)
	if err == nil {
		t.Fatal("expected error for (insecure=true, caBundle!=nil), got nil")
	}
}

// TestNewClientWithHTTPClient_NilHTTPClient verifies the explicit-client
// constructor rejects a nil client (programming bug — the whole point of the
// function is the explicit client).
func TestNewClientWithHTTPClient_NilHTTPClient(t *testing.T) {
	t.Parallel()
	_, err := NewClientWithHTTPClient(context.Background(), "https://127.0.0.1:1", "u", "p", true, nil)
	if err == nil {
		t.Fatal("expected error for nil httpClient, got nil")
	}
}

// TestNewClientWithHTTPClient_UsesSuppliedClient verifies the supplied client
// is actually wired through to gofish (not silently discarded as the SEC-5
// shim previously did). We point at an httptest TLS server with a self-signed
// cert; the supplied client trusts the cert via its own Transport, so the
// connection succeeds. A blank baseline call (default transport, no trust)
// would fail with x509 unknown authority.
func TestNewClientWithHTTPClient_UsesSuppliedClient(t *testing.T) {
	t.Parallel()
	// gofish.ConnectContext expects a 200 from /redfish/v1/ with at least an
	// empty service-root payload; respond accordingly. We don't need to fully
	// emulate Redfish — we only need to prove the *http.Client we pass is the
	// one used.
	mux := http.NewServeMux()
	mux.HandleFunc("/redfish/v1/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/","Systems":{"@odata.id":"/redfish/v1/Systems"}}`))
	})
	mux.HandleFunc("/redfish/v1/SessionService/Sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	server := httptest.NewTLSServer(mux)
	defer server.Close()

	// Build a client that trusts ONLY the test server's cert. Default
	// http.DefaultClient would not trust this cert.
	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// gofish may still error (no auth, mock server doesn't fully implement
	// Redfish) — but that error path is reached only if the TLS handshake
	// succeeded, which proves our client was used. The failure mode we want
	// to rule out is x509 "unknown authority", which would indicate the
	// supplied client was discarded.
	_, err := NewClientWithHTTPClient(ctx, server.URL, "u", "p", false, httpClient)
	if err != nil {
		// Acceptable: gofish reaches the server (TLS verified) and returns
		// a Redfish-level error (404, 500, "no Service{}", etc.).
		// Unacceptable: x509 / TLS verification failure — that would mean
		// the supplied client was discarded and a fresh, untrusting one
		// was built instead.
		errMsg := err.Error()
		for _, sub := range []string{"x509", "certificate signed by unknown authority", "tls: failed to verify"} {
			if strings.Contains(errMsg, sub) {
				t.Fatalf("supplied http.Client was not used (TLS verification failed): %v", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// MockClient.ForcePowerOff
// ---------------------------------------------------------------------------

func TestMockClient_ForcePowerOff_Success(t *testing.T) {
	t.Parallel()
	m := NewMockClient()
	ctx := context.Background()

	if err := m.ForcePowerOff(ctx); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if !m.ForcePowerOffCalled {
		t.Fatal("expected ForcePowerOffCalled=true")
	}
}

func TestMockClient_ForcePowerOff_ShouldFail(t *testing.T) {
	t.Parallel()
	m := NewMockClient()
	want := errors.New("bmc unavailable")
	m.ShouldFail["ForcePowerOff"] = want
	ctx := context.Background()

	err := m.ForcePowerOff(ctx)
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
	if !m.ForcePowerOffCalled {
		t.Fatal("expected ForcePowerOffCalled=true even on failure")
	}
}
