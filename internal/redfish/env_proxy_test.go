package redfish

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// D-035: a connection to a BMC ignores HTTP_PROXY and HTTPS_PROXY. Go's default
// transport honours them (http.ProxyFromEnvironment), which would let the
// environment decide who resolves a BMC's name (defeating D-032) and which
// intermediary carries plain-http:// credentials.
//
// The environment is read once per process: net/http caches it behind a
// sync.Once the first time any transport asks. A test that sets the variables
// with t.Setenv therefore passes or fails according to whether an earlier test
// in the package made a request through a default-proxy transport, and proves
// nothing when it passes in a full run. So the scenarios run in a child
// process that is started with the variables already set, as the manager is.
//
// ProxyFromEnvironment also never proxies a loopback address, so the BMC here
// is addressed as example.com (the name httptest's certificate is valid for)
// and the child's dialer, standing in for DNS, sends that name to the test
// server. A control scenario shows the setup does catch a transport that
// reads the environment.

const (
	envProxyChildMode = "B7_ENVPROXY_CHILD_MODE"
	envProxyAddress   = "B7_ENVPROXY_ADDRESS"
	envProxyReal      = "B7_ENVPROXY_REAL"
	envProxyCA        = "B7_ENVPROXY_CA"

	bmcTestHost     = "example.com"
	bmcTestUsername = "proxy-user"
	bmcTestPassword = "proxy-password"
)

// proxied is one request a recordingProxy received.
type proxied struct {
	method             string
	target             string // CONNECT: host:port; otherwise the request URL
	authorization      string
	proxyAuthorization string
}

// recordingProxy is an HTTP proxy that records every request it is asked to
// relay, and relays it to routes[target], where target is the host:port the
// client asked for. CONNECT is tunnelled, so TLS stays end to end.
type recordingProxy struct {
	*httptest.Server
	routes map[string]string

	mu   sync.Mutex
	seen []proxied
}

func newRecordingProxy(t *testing.T, routes map[string]string) *recordingProxy {
	t.Helper()
	p := &recordingProxy{routes: routes}
	p.Server = httptest.NewUnstartedServer(p)
	p.Start()
	t.Cleanup(p.Close)
	return p
}

// newRecordingTLSProxy is an https:// proxy serving cert.
func newRecordingTLSProxy(t *testing.T, routes map[string]string, cert tls.Certificate) *recordingProxy {
	t.Helper()
	p := &recordingProxy{routes: routes}
	p.Server = httptest.NewUnstartedServer(p)
	p.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	// A client that refuses the proxy's certificate is the point of some tests;
	// the server's note of the failed handshake is noise.
	p.Config.ErrorLog = stdlog.New(io.Discard, "", 0)
	p.StartTLS()
	t.Cleanup(p.Close)
	return p
}

func (p *recordingProxy) url() *url.URL {
	u, err := url.Parse(p.URL)
	if err != nil {
		panic(err)
	}
	return u
}

func (p *recordingProxy) requests() []proxied {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxied(nil), p.seen...)
}

func (p *recordingProxy) count() int { return len(p.requests()) }

func (p *recordingProxy) authorizations() int {
	n := 0
	for _, r := range p.requests() {
		if r.authorization != "" {
			n++
		}
	}
	return n
}

func (p *recordingProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.URL.String()
	if r.Method == http.MethodConnect {
		target = r.Host
	}
	p.mu.Lock()
	p.seen = append(p.seen, proxied{
		method:             r.Method,
		target:             target,
		authorization:      r.Header.Get("Authorization"),
		proxyAuthorization: r.Header.Get("Proxy-Authorization"),
	})
	p.mu.Unlock()

	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	real, ok := p.routes[r.URL.Host]
	if !ok {
		http.Error(w, "no route", http.StatusBadGateway)
		return
	}
	(&httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(&url.URL{Scheme: "http", Host: real})
		pr.Out.Host = pr.In.Host
	}}).ServeHTTP(w, r)
}

func (p *recordingProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	real, ok := p.routes[r.Host]
	if !ok {
		http.Error(w, "no route", http.StatusBadGateway)
		return
	}
	upstream, err := net.Dial("tcp", real)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	done := make(chan struct{}, 2)
	relay := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go relay(upstream, client)
	go relay(client, upstream)
	<-done
	<-done
	_ = client.Close()
	_ = upstream.Close()
}

// bmcFixture is a TLS BMC reachable by name only through a dialer that maps
// bmcTestHost to it.
type bmcFixture struct {
	server  *httptest.Server
	log     *requestLog
	port    string
	address string // https://example.com:<port>
	routes  map[string]string
}

func newBMCFixture(t *testing.T) *bmcFixture {
	t.Helper()
	log := &requestLog{}
	srv := httptest.NewTLSServer(fakeBMC(log, "/redfish/v1/Systems", nil))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	name := net.JoinHostPort(bmcTestHost, port)
	return &bmcFixture{
		server:  srv,
		log:     log,
		port:    port,
		address: "https://" + name,
		routes:  map[string]string{name: srv.Listener.Addr().String()},
	}
}

// addressAs is the BMC's address under another host name, and the proxy routes
// for it, for tests that skip certificate verification and must not resolve a
// real name.
func (f *bmcFixture) addressAs(host string) (string, map[string]string) {
	name := net.JoinHostPort(host, f.port)
	return "https://" + name, map[string]string{name: f.server.Listener.Addr().String()}
}

// caPEM is the bundle that makes the BMC's certificate verify for bmcTestHost.
func (f *bmcFixture) caPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
}

// nameDial sends bmcTestHost, in either spelling, to real, and everything else
// where it was going.
func nameDial(real string) dialContextFunc {
	var d net.Dialer
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(address); err == nil && strings.TrimSuffix(host, ".") == bmcTestHost {
			address = real
		}
		return d.DialContext(ctx, network, address)
	}
}

func TestBMCClientIgnoresTheEnvironmentProxy(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		caBundle  bool
		wantProxy bool
	}{
		// The control: a transport that does read the environment must reach the
		// proxy, or the other scenarios could pass for the wrong reason.
		{mode: "control", wantProxy: true},
		// NewClient with no CA bundle, and with one: the two paths through
		// newHTTPClient that a BMC connection takes.
		{mode: "client"},
		{mode: "client", caBundle: true},
	} {
		name := tc.mode
		if tc.caBundle {
			name += "/custom-ca"
		}
		t.Run(name, func(t *testing.T) {
			bmc := newBMCFixture(t)
			proxy := newRecordingProxy(t, bmc.routes)

			cmd := exec.Command(os.Args[0], "-test.run=^TestEnvProxyChild$", "-test.count=1")
			cmd.Env = append(withoutProxyVars(os.Environ()),
				"HTTP_PROXY="+proxy.URL,
				"HTTPS_PROXY="+proxy.URL,
				"http_proxy="+proxy.URL,
				"https_proxy="+proxy.URL,
				envProxyChildMode+"="+tc.mode,
				envProxyAddress+"="+bmc.address,
				envProxyReal+"="+bmc.server.Listener.Addr().String(),
			)
			if tc.caBundle {
				cmd.Env = append(cmd.Env, envProxyCA+"="+string(bmc.caPEM()))
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the client did not reach the BMC: %v\n%s", err, out)
			}
			if tc.wantProxy && proxy.count() == 0 {
				t.Fatalf("the control transport never reached the proxy, so this setup cannot detect one that reads the environment\n%s", out)
			}
			if !tc.wantProxy && proxy.count() != 0 {
				t.Fatalf("the environment's proxy was used for a BMC connection: %+v", proxy.requests())
			}
		})
	}
}

func withoutProxyVars(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch strings.ToUpper(strings.SplitN(kv, "=", 2)[0]) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// TestEnvProxyChild is the child process of TestBMCClientIgnoresTheEnvironmentProxy.
// It does nothing in a normal run.
func TestEnvProxyChild(t *testing.T) {
	mode := os.Getenv(envProxyChildMode)
	if mode == "" {
		t.Skip("child process of TestBMCClientIgnoresTheEnvironmentProxy")
	}
	address, real, caPEM := os.Getenv(envProxyAddress), os.Getenv(envProxyReal), os.Getenv(envProxyCA)
	dial := nameDial(real)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if mode == "control" {
		transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dial,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- test fixture certificate
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/redfish/v1/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			t.Fatalf("control request: %v", err)
		}
		_ = resp.Body.Close()
		return
	}

	// NewClient is the shipped constructor; only the dialer standing in for DNS
	// is replaced.
	systemDial = dial
	var bundle []byte
	if caPEM != "" {
		bundle = []byte(caPEM)
	}
	c, err := NewClient(ctx, address, bmcTestUsername, bmcTestPassword, bundle == nil, bundle)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close(ctx)
	if _, err := c.GetSystemInfo(ctx); err != nil {
		t.Fatalf("GetSystemInfo: %v", err)
	}
}
