package redfish

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// D-035: with --bmc-proxy set, every connection to a BMC goes through that one
// proxy. The environment is never read, so these specs name the proxy
// explicitly and need no process-wide setting. The BMC is addressed as
// example.com, which nothing here can resolve to it: the proxy, as a real one
// would, is what knows where that name goes.

// proxyOnlyDial is a dialer that connects to the proxy and refuses every other
// address. A transport that skips the proxy then fails here, offline, instead
// of resolving the BMC's name; and what it was asked to dial is on record,
// which is how the specs show the manager never looks the BMC's name up.
type proxyOnlyDial struct {
	allowed string

	mu    sync.Mutex
	addrs []string
}

func newProxyOnlyDial(p *bmcProxy) *proxyOnlyDial {
	return &proxyOnlyDial{allowed: net.JoinHostPort(p.url.Hostname(), p.url.Port())}
}

func (d *proxyOnlyDial) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, address)
	d.mu.Unlock()
	if address != d.allowed {
		return nil, fmt.Errorf("unexpected dial of %q: only the proxy %q may be dialed", address, d.allowed)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, address)
}

func (d *proxyOnlyDial) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// getSystemInfoThrough connects to address with newClient through proxy and
// reads the system, which is a Redfish service-root walk and an authenticated
// read. It returns the addresses the client dialed.
func getSystemInfoThrough(t *testing.T, proxy *bmcProxy, address string, insecure bool, caBundle []byte) ([]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dialer := newProxyOnlyDial(proxy)
	c, err := newClient(ctx, dialer.dial, proxy, address, bmcTestUsername, bmcTestPassword, insecure, caBundle)
	if err != nil {
		return dialer.dialed(), err
	}
	defer c.Close(ctx)
	_, err = c.GetSystemInfo(ctx)
	return dialer.dialed(), err
}

// proxyAuthorizations counts the requests l received that carry a
// Proxy-Authorization header.
func proxyAuthorizations(l *requestLog) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, h := range l.seen {
		if h.Get("Proxy-Authorization") != "" {
			n++
		}
	}
	return n
}

func TestBMCProxy_CarriesAnHTTPSBMCEndToEnd(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		caBundle bool
	}{
		{name: "default client"},
		{name: "custom CA", caBundle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bmc := newBMCFixture(t)
			proxy := newRecordingProxy(t, bmc.routes)
			var ca []byte
			if tc.caBundle {
				ca = bmc.caPEM()
			}

			dialed, err := getSystemInfoThrough(t, newBMCProxy(proxy.url()), bmc.address, !tc.caBundle, ca)
			if err != nil {
				t.Fatalf("a BMC behind the proxy must be usable: %v", err)
			}

			seen := proxy.requests()
			if len(seen) == 0 {
				t.Fatal("the proxy was never used")
			}
			for _, r := range seen {
				if r.method != http.MethodConnect || r.target != net.JoinHostPort(bmcTestHost, bmc.port) {
					t.Fatalf("the proxy saw %+v, want only CONNECT %s:%s", r, bmcTestHost, bmc.port)
				}
			}
			// TLS stays end to end: the credentials are inside the tunnel.
			if n := proxy.authorizations(); n != 0 {
				t.Fatalf("the proxy saw the BMC credentials on %d request(s)", n)
			}
			if bmc.log.authorizations() == 0 {
				t.Fatal("the BMC never received the credentials")
			}
			// The manager dials the proxy and nothing else: the proxy resolves
			// the BMC's name, which is where D-032's absolute-name dial stops
			// applying.
			for _, addr := range dialed {
				if addr != proxy.Listener.Addr().String() {
					t.Fatalf("the manager dialed %q; only the proxy %q may be dialed", addr, proxy.Listener.Addr())
				}
			}
		})
	}
}

// A plain-http:// BMC is already behind the bmc-insecure-transport opt-in
// (D-030). Through a proxy its credentials cross the proxy in clear; the docs
// say so, and this is the behaviour they describe.
func TestBMCProxy_PlainHTTPBMCCredentialsCrossTheProxy(t *testing.T) {
	t.Parallel()
	log := &requestLog{}
	bmc := httptest.NewServer(fakeBMC(log, "/redfish/v1/Systems", nil))
	defer bmc.Close()
	_, port, err := net.SplitHostPort(bmc.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	name := net.JoinHostPort(bmcTestHost, port)
	proxy := newRecordingProxy(t, map[string]string{name: bmc.Listener.Addr().String()})

	if _, err := getSystemInfoThrough(t, newBMCProxy(proxy.url()), "http://"+name, false, nil); err != nil {
		t.Fatalf("a plain-http BMC behind the proxy must be usable: %v", err)
	}
	if proxy.authorizations() == 0 {
		t.Fatal("expected the proxy to see the Authorization header of a plain-http BMC request")
	}
	if log.authorizations() == 0 {
		t.Fatal("the BMC never received the credentials")
	}
}

func TestBMCProxy_UserinfoIsSentAsProxyAuthorizationOnly(t *testing.T) {
	t.Parallel()
	bmc := newBMCFixture(t)
	proxy := newRecordingProxy(t, bmc.routes)
	withAuth := proxy.url()
	withAuth.User = url.UserPassword("proxy-operator", "proxy-secret")

	if _, err := getSystemInfoThrough(t, newBMCProxy(withAuth), bmc.address, true, nil); err != nil {
		t.Fatalf("connect: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-operator:proxy-secret"))
	seen := proxy.requests()
	if len(seen) == 0 {
		t.Fatal("the proxy was never used")
	}
	for _, r := range seen {
		if r.proxyAuthorization != want {
			t.Fatalf("CONNECT carried Proxy-Authorization %q, want %q", r.proxyAuthorization, want)
		}
	}
	// The proxy's credentials never reach the BMC.
	if n := proxyAuthorizations(bmc.log); n != 0 {
		t.Fatalf("the BMC received the proxy's credentials on %d request(s)", n)
	}
}

// The errors a refused CONNECT produces end up in conditions and logs. They
// must not carry the proxy's user name or password, or the BMC's.
func TestBMCProxy_ErrorsDoNotCarryCredentials(t *testing.T) {
	t.Parallel()
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
	}))
	defer refusing.Close()
	proxyURL, err := url.Parse(refusing.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword("proxy-operator", "proxy-secret")

	_, err = getSystemInfoThrough(t, newBMCProxy(proxyURL), "https://bmc.invalid:8443", true, nil)
	if err == nil || !strings.Contains(err.Error(), "Proxy Authentication Required") {
		t.Fatalf("a proxy that refuses the CONNECT must fail the call with its refusal, got %v", err)
	}
	for _, secret := range []string{"proxy-operator", "proxy-secret", bmcTestUsername, bmcTestPassword,
		base64.StdEncoding.EncodeToString([]byte("proxy-operator:proxy-secret"))} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error carries %q: %v", secret, err)
		}
	}
}

// SEC-17c still holds under a proxy: the origin pinning sits above the
// transport, so a request off the BMC's origin is refused before the proxy
// is asked for it.
func TestBMCProxy_OriginPinningStillApplies(t *testing.T) {
	t.Parallel()
	log := &requestLog{}
	srv := httptest.NewTLSServer(fakeBMC(log, "@attacker.example:8443/redfish/v1/Systems", nil))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	name := net.JoinHostPort(bmcTestHost, port)
	proxy := newRecordingProxy(t, map[string]string{name: srv.Listener.Addr().String(), "attacker.example:8443": srv.Listener.Addr().String()})

	_, err = getSystemInfoThrough(t, newBMCProxy(proxy.url()), "https://"+name, true, nil)
	if err == nil || !strings.Contains(err.Error(), "refused a request") {
		t.Fatalf("an @odata.id that leaves the BMC's origin must be refused by the pinning, got %v", err)
	}
	for _, r := range proxy.requests() {
		if strings.Contains(r.target, "attacker") {
			t.Fatalf("the proxy was asked for %q", r.target)
		}
	}
}

// An https:// proxy is verified on its own terms. A transport has one TLS
// configuration, which Go would apply to the proxy as well as to the BMC: the
// proxy would then go unverified whenever the BMC's host has
// insecureSkipVerify, and be checked against the BMC's CA bundle otherwise.
func TestBMCProxy_HTTPSProxyIsVerifiedWithTheSystemRoots(t *testing.T) {
	t.Parallel()
	bmc := newBMCFixture(t)
	proxyCAPEM, proxyCert := generateSelfSignedCA(t)
	proxy := newRecordingTLSProxy(t, bmc.routes, proxyCert)
	proxyURL := proxy.url()
	// The test certificate is valid for the name "localhost".
	_, proxyPort, err := net.SplitHostPort(proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.Host = net.JoinHostPort("localhost", proxyPort)
	proxyRoots := x509.NewCertPool()
	if !proxyRoots.AppendCertsFromPEM(proxyCAPEM) {
		t.Fatal("no certificate in the proxy CA")
	}

	t.Run("an untrusted proxy is refused even where the BMC skips verification", func(t *testing.T) {
		_, err := getSystemInfoThrough(t, newBMCProxy(proxyURL), bmc.address, true, nil)
		var unknown x509.UnknownAuthorityError
		if !errors.As(err, &unknown) {
			t.Fatalf("expected the proxy's certificate to be refused, got %v", err)
		}
		if n := proxy.count(); n != 0 {
			t.Fatalf("the proxy received %d request(s) over a connection that failed verification", n)
		}
	})

	t.Run("a trusted proxy carries a BMC that skips verification", func(t *testing.T) {
		p := &bmcProxy{url: proxyURL, roots: proxyRoots}
		if _, err := getSystemInfoThrough(t, p, bmc.address, true, nil); err != nil {
			t.Fatalf("connect: %v", err)
		}
		if proxy.count() == 0 {
			t.Fatal("the proxy was never used")
		}
	})

	t.Run("the BMC's CA bundle is not what verifies the proxy", func(t *testing.T) {
		p := &bmcProxy{url: proxyURL, roots: proxyRoots}
		if _, err := getSystemInfoThrough(t, p, bmc.address, false, bmc.caPEM()); err != nil {
			t.Fatalf("connect: %v", err)
		}
	})
}

func TestNewClientFactory(t *testing.T) {
	t.Parallel()

	t.Run("no proxy is NewClient", func(t *testing.T) {
		t.Parallel()
		if reflect.ValueOf(NewClientFactory(nil)).Pointer() != reflect.ValueOf(NewClient).Pointer() {
			t.Fatal("a nil proxy must give the direct constructor")
		}
	})

	t.Run("the proxy resolves the name, so the shipped dialer reaches the BMC", func(t *testing.T) {
		t.Parallel()
		bmc := newBMCFixture(t)
		// A name that no resolver answers: only the proxy can know where it goes.
		address, routes := bmc.addressAs("bmc.invalid")
		proxy := newRecordingProxy(t, routes)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		c, err := NewClientFactory(proxy.url())(ctx, address, bmcTestUsername, bmcTestPassword, true, nil)
		if err != nil {
			t.Fatalf("factory: %v", err)
		}
		defer c.Close(ctx)
		if _, err := c.GetSystemInfo(ctx); err != nil {
			t.Fatalf("GetSystemInfo: %v", err)
		}
		if proxy.count() == 0 {
			t.Fatal("the proxy was never used")
		}
	})

	t.Run("the factory keeps its own copy of the URL", func(t *testing.T) {
		t.Parallel()
		bmc := newBMCFixture(t)
		address, routes := bmc.addressAs("bmc.invalid")
		proxy := newRecordingProxy(t, routes)
		u := proxy.url()
		factory := NewClientFactory(u)
		u.Host = "elsewhere.invalid:3128"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		c, err := factory(ctx, address, bmcTestUsername, bmcTestPassword, true, nil)
		if err != nil {
			t.Fatalf("factory: %v", err)
		}
		c.Close(ctx)
		if proxy.count() == 0 {
			t.Fatal("changing the caller's URL moved the proxy")
		}
	})
}

func TestBMCTransport_Proxy(t *testing.T) {
	t.Parallel()

	t.Run("direct has no proxy function", func(t *testing.T) {
		t.Parallel()
		c, err := newHTTPClient(testEndpoint, false, nil, systemDial, nil)
		if err != nil {
			t.Fatal(err)
		}
		tr := bmcTransport(t, c)
		if tr.Proxy != nil {
			t.Fatal("a direct BMC transport must not consult any proxy, the environment's included")
		}
		if tr.DialTLSContext != nil {
			t.Fatal("a direct BMC transport has no separate TLS dialer")
		}
	})

	t.Run("a proxied transport sends everything to the one proxy", func(t *testing.T) {
		t.Parallel()
		proxyURL, err := url.Parse("http://proxy.example:3128")
		if err != nil {
			t.Fatal(err)
		}
		c, err := newHTTPClient(testEndpoint, false, nil, systemDial, newBMCProxy(proxyURL))
		if err != nil {
			t.Fatal(err)
		}
		tr := bmcTransport(t, c)
		if tr.Proxy == nil {
			t.Fatal("no proxy function")
		}
		// Unlike http.ProxyFromEnvironment, loopback is not exempt.
		for _, target := range []string{"https://bmc.example.com/redfish/v1/", "http://10.0.0.5/", "http://127.0.0.1:8000/", "https://localhost/"} {
			req, err := http.NewRequest(http.MethodGet, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tr.Proxy(req)
			if err != nil || got == nil || got.String() != proxyURL.String() {
				t.Fatalf("Proxy(%s) = %v, %v; want %v", target, got, err, proxyURL)
			}
		}
		if tr.DialTLSContext != nil {
			t.Fatal("an http:// proxy needs no separate TLS dialer")
		}
	})

	t.Run("an https proxy has its own TLS dialer", func(t *testing.T) {
		t.Parallel()
		proxyURL, err := url.Parse("https://proxy.example:3128")
		if err != nil {
			t.Fatal(err)
		}
		c, err := newHTTPClient(testEndpoint, true, nil, systemDial, newBMCProxy(proxyURL))
		if err != nil {
			t.Fatal(err)
		}
		if bmcTransport(t, c).DialTLSContext == nil {
			t.Fatal("the hop to an https:// proxy would share the BMC's TLS configuration")
		}
	})
}

// A client handed in without a transport used http.DefaultTransport, which
// reads the environment.
func TestNewClientWithHTTPClient_NoTransportIsDirect(t *testing.T) {
	t.Parallel()
	bmc := httptest.NewServer(fakeBMC(&requestLog{}, "/redfish/v1/Systems", nil))
	defer bmc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := NewClientWithHTTPClient(ctx, bmc.URL, bmcTestUsername, bmcTestPassword, false, &http.Client{})
	if err != nil {
		t.Fatalf("NewClientWithHTTPClient: %v", err)
	}
	defer c.Close(ctx)

	pinned, ok := c.(*gofishClient).gofishClient.HTTPClient.Transport.(*originPinnedTransport)
	if !ok {
		t.Fatalf("expected the transport to be pinned, got %T", c.(*gofishClient).gofishClient.HTTPClient.Transport)
	}
	base, ok := pinned.base.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport under the pinning, got %T", pinned.base)
	}
	if base.Proxy != nil {
		t.Fatal("the fallback transport must not consult any proxy")
	}
	if base == http.DefaultTransport {
		t.Fatal("the fallback must be a copy; the shared default belongs to the whole process")
	}
	if http.DefaultTransport.(*http.Transport).Proxy == nil {
		t.Fatal("the process-wide default transport was changed")
	}
}
