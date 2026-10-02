package redfish

import (
	"bytes"
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// SEC-17(c): the credentials go to the origin the bmc-addresses annotation
// authorised (D-030) and nowhere else. gofish sends them as Basic auth on every
// request after the service root, builds each request URL by appending an
// @odata.id the BMC returned to the configured address, and leaves redirects to
// net/http, which keeps the Authorization header across a change of port or
// scheme and into subdomains. Every spec here drives the real client through
// NewClient, so what it proves is what reaches the wire.

const (
	pinTestUsername = "pinned-user"
	pinTestPassword = "pinned-password"
)

// requestLog records the requests a test server received.
type requestLog struct {
	mu   sync.Mutex
	seen []http.Header
}

func (l *requestLog) record(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, r.Header.Clone())
}

func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

func (l *requestLog) authorizations() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, h := range l.seen {
		if h.Get("Authorization") != "" {
			n++
		}
	}
	return n
}

// fakeBMC serves the smallest Redfish tree GetSystemInfo walks. systemsLink is
// the @odata.id the service root gives for the Systems collection, and
// redirects maps a path to the Location it answers with a 307.
func fakeBMC(log *requestLog, systemsLink string, redirects map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		if loc, ok := redirects[r.URL.Path]; ok {
			http.Redirect(w, r, loc, http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimSuffix(r.URL.Path, "/") {
		case "/redfish/v1":
			_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/","Systems":{"@odata.id":"` + systemsLink + `"}}`))
		case "/redfish/v1/Systems":
			_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/Systems","Members":[{"@odata.id":"/redfish/v1/Systems/1"}],"Members@odata.count":1}`))
		case "/redfish/v1/Systems/1":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/Systems/1","Id":"1","Manufacturer":"Acme","Model":"Pinned","PowerState":"On"}`))
		default:
			http.NotFound(w, r)
		}
	})
}

// attackerServer is an endpoint the bmc-addresses annotation never named.
func attackerServer(t *testing.T) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func connectAndQuery(t *testing.T, address string, caBundle []byte) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := NewClient(ctx, address, pinTestUsername, pinTestPassword, false, caBundle)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	_, err = c.GetSystemInfo(ctx)
	return err
}

func expectNoCredentialLeak(t *testing.T, err error) {
	t.Helper()
	if err != nil && (strings.Contains(err.Error(), pinTestPassword) || strings.Contains(err.Error(), pinTestUsername)) {
		t.Fatalf("the error carries the credentials: %v", err)
	}
}

func TestOriginPinning_SameOriginRequestsAndRedirectsAreSent(t *testing.T) {
	t.Parallel()
	log := &requestLog{}
	bmc := httptest.NewServer(fakeBMC(log, "/redfish/v1/Systems", map[string]string{
		// A redirect within the origin, as a BMC that canonicalises a
		// trailing slash would send, is still followed.
		"/redfish/v1/Systems": "/redfish/v1/Systems/",
	}))
	defer bmc.Close()

	if err := connectAndQuery(t, bmc.URL, nil); err != nil {
		t.Fatalf("a BMC that stays on its own origin must be usable: %v", err)
	}
	if log.authorizations() == 0 {
		t.Fatal("the BMC never received the credentials")
	}
}

func TestOriginPinning_RedirectToAnotherPortIsRefused(t *testing.T) {
	t.Parallel()
	attacker, attackerLog := attackerServer(t)
	bmcLog := &requestLog{}
	// Same host (127.0.0.1), another port: net/http keeps Authorization.
	bmc := httptest.NewServer(fakeBMC(bmcLog, "/redfish/v1/Systems", map[string]string{
		"/redfish/v1/Systems": attacker.URL + "/redfish/v1/Systems",
	}))
	defer bmc.Close()

	err := connectAndQuery(t, bmc.URL, nil)
	if err == nil {
		t.Fatal("a redirect off the BMC's origin must fail the call")
	}
	expectNoCredentialLeak(t, err)
	if n := attackerLog.count(); n != 0 {
		t.Fatalf("the client followed a redirect to another port: %d request(s) reached it, %d with Authorization",
			n, attackerLog.authorizations())
	}
}

func TestOriginPinning_RedirectToAnotherHostIsRefused(t *testing.T) {
	t.Parallel()
	attacker, attackerLog := attackerServer(t)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(attacker.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	bmcLog := &requestLog{}
	bmc := httptest.NewServer(fakeBMC(bmcLog, "/redfish/v1/Systems", map[string]string{
		"/redfish/v1/Systems": "http://localhost:" + port + "/redfish/v1/Systems",
	}))
	defer bmc.Close()

	if err := connectAndQuery(t, bmc.URL, nil); err == nil {
		t.Fatal("a redirect to another host must fail the call")
	}
	if n := attackerLog.count(); n != 0 {
		t.Fatalf("the client followed a redirect to another host: %d request(s) reached it", n)
	}
}

// An @odata.id is appended to the configured address as text, so one that
// starts with "@" turns the address into userinfo and names a new host.
func TestOriginPinning_ODataIDNamingAnotherHostIsRefused(t *testing.T) {
	t.Parallel()
	attacker, attackerLog := attackerServer(t)
	bmcLog := &requestLog{}
	bmc := httptest.NewServer(fakeBMC(bmcLog,
		"@"+strings.TrimPrefix(attacker.URL, "http://")+"/redfish/v1/Systems", nil))
	defer bmc.Close()

	err := connectAndQuery(t, bmc.URL, nil)
	if err == nil {
		t.Fatal("an @odata.id that leaves the BMC's origin must fail the call")
	}
	expectNoCredentialLeak(t, err)
	if n := attackerLog.count(); n != 0 {
		t.Fatalf("an @odata.id from the BMC sent %d request(s), %d with Authorization, to another host",
			n, attackerLog.authorizations())
	}
}

// rawCapture records every byte the server reads off the wire, before TLS.
type rawCapture struct {
	net.Listener
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *rawCapture) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &capturedConn{Conn: c, capture: l}, nil
}

func (l *rawCapture) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.buf.Bytes())
}

type capturedConn struct {
	net.Conn
	capture *rawCapture
}

func (c *capturedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.capture.mu.Lock()
	c.capture.buf.Write(p[:n])
	c.capture.mu.Unlock()
	return n, err
}

// A redirect from https:// to http:// on the same host and port: net/http
// follows it and writes the Authorization header in plaintext.
func TestOriginPinning_RedirectToPlainHTTPIsRefused(t *testing.T) {
	t.Parallel()
	bmcLog := &requestLog{}
	srv := httptest.NewUnstartedServer(nil)
	capture := &rawCapture{Listener: srv.Listener}
	srv.Listener = capture
	srv.Config.Handler = fakeBMC(bmcLog, "/redfish/v1/Systems", map[string]string{
		"/redfish/v1/Systems": "http://" + capture.Addr().String() + "/redfish/v1/Systems",
	})
	srv.StartTLS()
	defer srv.Close()
	caBundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	err := connectAndQuery(t, srv.URL, caBundle)
	if err == nil {
		t.Fatal("a redirect from https to http must fail the call")
	}
	expectNoCredentialLeak(t, err)
	if raw := capture.bytes(); bytes.Contains(bytes.ToLower(raw), []byte("authorization:")) {
		t.Fatalf("the credentials were written in plaintext after a redirect to http://")
	}
}

// countingRoundTripper stands in for the network under the pinning.
type countingRoundTripper struct {
	mu    sync.Mutex
	calls int
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestOriginPinnedTransport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		endpoint string
		request  string
		allowed  bool
	}{
		{"https://bmc.example.com:8443", "https://bmc.example.com:8443/redfish/v1/Systems", true},
		{"https://bmc.example.com:8443", "https://BMC.Example.com:8443/redfish/v1/", true},
		{"https://bmc.example.com", "https://bmc.example.com:443/redfish/v1/", true},
		{"https://bmc.example.com:443", "https://bmc.example.com/redfish/v1/", true},
		{"http://10.0.0.5", "http://10.0.0.5:80/redfish/v1/", true},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443/redfish/v1/", true},
		// Another scheme on the same host and port.
		{"https://bmc.example.com:8443", "http://bmc.example.com:8443/redfish/v1/", false},
		// Another port, including the scheme's default.
		{"https://bmc.example.com:8443", "https://bmc.example.com:9443/redfish/v1/", false},
		{"https://bmc.example.com:8443", "https://bmc.example.com/redfish/v1/", false},
		// A subdomain: net/http keeps Authorization on a redirect there.
		{"https://bmc.example.com:8443", "https://a.bmc.example.com:8443/redfish/v1/", false},
		{"https://bmc.example.com:8443", "https://bmc.example.com.attacker.example:8443/redfish/v1/", false},
		{"https://10.0.0.5", "https://10.0.0.6/redfish/v1/", false},
		// The shape an @odata.id starting with "@" gives the URL.
		{"https://10.0.0.5", "https://10.0.0.5@10.0.0.5/redfish/v1/", false},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint+" "+tc.request, func(t *testing.T) {
			t.Parallel()
			endpoint, err := url.Parse(tc.endpoint)
			if err != nil {
				t.Fatal(err)
			}
			base := &countingRoundTripper{}
			pinned, err := pinToOrigin(base, endpoint)
			if err != nil {
				t.Fatalf("pinToOrigin: %v", err)
			}
			req, err := http.NewRequest(http.MethodGet, tc.request, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Basic cGlubmVkLXVzZXI6cGlubmVkLXBhc3N3b3Jk")
			resp, err := pinned.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if tc.allowed {
				if err != nil || base.calls != 1 {
					t.Fatalf("a request to the BMC's own origin was refused: %v", err)
				}
				return
			}
			if err == nil || base.calls != 0 {
				t.Fatalf("a request off the BMC's origin reached the network")
			}
			if strings.Contains(err.Error(), "cGlubmVk") {
				t.Fatalf("the refusal repeats the Authorization header: %v", err)
			}
		})
	}
}

// NewClientWithHTTPClient keeps the caller's transport, under the same pinning.
func TestNewClientWithHTTPClient_IsPinnedToTheAddress(t *testing.T) {
	t.Parallel()
	attacker, attackerLog := attackerServer(t)
	bmcLog := &requestLog{}
	bmc := httptest.NewServer(fakeBMC(bmcLog,
		"@"+strings.TrimPrefix(attacker.URL, "http://")+"/redfish/v1/Systems", nil))
	defer bmc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := NewClientWithHTTPClient(ctx, bmc.URL, pinTestUsername, pinTestPassword, false, bmc.Client())
	if err != nil {
		t.Fatalf("NewClientWithHTTPClient: %v", err)
	}
	defer c.Close(ctx)
	if _, err := c.GetSystemInfo(ctx); err == nil {
		t.Fatal("an @odata.id that leaves the BMC's origin must fail the call")
	}
	if n := attackerLog.count(); n != 0 {
		t.Fatalf("%d request(s), %d with Authorization, reached another host", n, attackerLog.authorizations())
	}
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}

func TestOriginPinnedTransport_ClosesARefusedRequestBody(t *testing.T) {
	t.Parallel()
	pinned, err := pinToOrigin(&countingRoundTripper{}, &url.URL{Scheme: "https", Host: "10.0.0.5"})
	if err != nil {
		t.Fatal(err)
	}
	body := &closeTracker{Reader: strings.NewReader(`{"ResetType":"On"}`)}
	req, err := http.NewRequest(http.MethodPost, "https://203.0.113.9/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pinned.RoundTrip(req); err == nil {
		t.Fatal("expected the request to be refused")
	}
	if !body.closed {
		t.Fatal("a RoundTripper must close the body of a request it refuses")
	}
}

func TestPinToOriginNeedsAnOrigin(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"https:10.0.0.5", "10.0.0.5", "ftp://10.0.0.5", "https://"} {
		u, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pinToOrigin(&countingRoundTripper{}, u); err == nil {
			t.Errorf("pinToOrigin(%q) pinned to an address with no http(s) origin", endpoint)
		}
	}
}
