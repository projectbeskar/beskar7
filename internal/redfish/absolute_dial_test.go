package redfish

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// testEndpoint is a BMC address for tests that never connect.
var testEndpoint = &url.URL{Scheme: "https", Host: "bmc.example.com"}

// D-032 (SEC-17a): a BMC hostname is dialed as an absolute DNS name. Resolved
// through the manager pod's search path (ndots:5), bmc1.lab is tried as
// bmc1.lab.<namespace>.svc.cluster.local before bmc1.lab itself, so whoever
// can create a Service named bmc1 in a namespace called lab receives the
// connection, and the credentials the bmc-addresses annotation authorised for
// the real bmc1.lab.

var errDialRecorded = errors.New("dial recorded, not connected")

// dialRecorder records the addresses the transport dials. With target set it
// connects there instead, so a request can complete without DNS.
type dialRecorder struct {
	mu     sync.Mutex
	dialed []string
	target string
}

func (d *dialRecorder) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, address)
	d.mu.Unlock()
	if d.target == "" {
		return nil, errDialRecorded
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, d.target)
}

func (d *dialRecorder) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.dialed)
}

func getThrough(t *testing.T, c *http.Client, rawURL string) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(req)
}

func TestBMCHostnameIsDialedAsAnAbsoluteName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		address string
		want    string
	}{
		{"https://bmc1.lab:8443", "bmc1.lab.:8443"},
		{"https://bmc1", "bmc1.:443"},
		{"http://bmc1.lab", "bmc1.lab.:80"},
		{"https://mock-redfish.beskar7-smoke.svc.cluster.local:8443", "mock-redfish.beskar7-smoke.svc.cluster.local.:8443"},
		{"https://BMC1.Lab", "BMC1.Lab.:443"},
		// IP literals have nothing to resolve.
		{"https://10.0.0.5", "10.0.0.5:443"},
		{"https://10.0.0.5:8443", "10.0.0.5:8443"},
		{"https://[2001:db8::1]:8443", "[2001:db8::1]:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.address, func(t *testing.T) {
			t.Parallel()
			endpoint, err := url.Parse(tc.address)
			if err != nil {
				t.Fatal(err)
			}
			rec := &dialRecorder{}
			c, err := newHTTPClient(endpoint, false, nil, rec.dial, nil)
			if err != nil {
				t.Fatalf("newHTTPClient: %v", err)
			}
			if _, err := getThrough(t, c, tc.address+"/redfish/v1/"); !errors.Is(err, errDialRecorded) {
				t.Fatalf("expected the recorded dial to fail the request, got %v", err)
			}
			if got := rec.addresses(); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("dialed %q, want [%q]", got, tc.want)
			}
		})
	}
}

func TestAbsoluteDialAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ bmcHost, address, want string }{
		{"bmc1.lab", "bmc1.lab:443", "bmc1.lab.:443"},
		{"BMC1.lab", "bmc1.LAB:443", "bmc1.LAB.:443"},
		{"bmc1.lab", "bmc1.lab.:443", "bmc1.lab.:443"},
		// The --bmc-proxy proxy is the operator's name to resolve.
		{"bmc1.lab", "proxy:3128", "proxy:3128"},
		{"10.0.0.5", "10.0.0.5:443", "10.0.0.5:443"},
		{"2001:db8::1", "[2001:db8::1]:443", "[2001:db8::1]:443"},
		{"bmc1.lab", "not-a-host-port", "not-a-host-port"},
	} {
		if got := absoluteDialAddress(tc.bmcHost, tc.address); got != tc.want {
			t.Errorf("absoluteDialAddress(%q, %q) = %q, want %q", tc.bmcHost, tc.address, got, tc.want)
		}
	}
}

// Only the dial changes: the Host header, and therefore the name the BMC sees
// and the name its certificate is checked against, stay as written.
func TestBMCHostnameDialLeavesTheRequestAlone(t *testing.T) {
	t.Parallel()
	var host string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	endpoint := &url.URL{Scheme: "http", Host: "bmc1.lab:8000"}
	rec := &dialRecorder{target: srv.Listener.Addr().String()}
	c, err := newHTTPClient(endpoint, false, nil, rec.dial, nil)
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	resp, err := getThrough(t, c, "http://bmc1.lab:8000/redfish/v1/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if host != "bmc1.lab:8000" {
		t.Fatalf("the BMC saw Host %q, want %q", host, "bmc1.lab:8000")
	}
	if got := rec.addresses(); !slices.Equal(got, []string{"bmc1.lab.:8000"}) {
		t.Fatalf("dialed %q", got)
	}
}

// fakeDNS answers every question with NXDOMAIN and records the names asked,
// which is what makes a resolver walk its whole search list.
type fakeDNS struct {
	conn net.PacketConn
	mu   sync.Mutex
	asks []string
}

func startFakeDNS(t *testing.T) *fakeDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDNS{conn: pc}
	t.Cleanup(func() { _ = pc.Close() })
	go f.serve()
	return f
}

func (f *fakeDNS) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		hdr, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		f.mu.Lock()
		f.asks = append(f.asks, q.Name.String())
		f.mu.Unlock()
		reply := dnsmessage.Message{
			Header: dnsmessage.Header{
				ID: hdr.ID, Response: true, RecursionDesired: hdr.RecursionDesired,
				RecursionAvailable: true, RCode: dnsmessage.RCodeNameError,
			},
			Questions: []dnsmessage.Question{q},
		}
		out, err := reply.Pack()
		if err != nil {
			continue
		}
		_, _ = f.conn.WriteTo(out, from)
	}
}

func (f *fakeDNS) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asks)
}

// resolver sends every DNS query to f, whatever nameserver the host's
// resolv.conf names. The search list still comes from resolv.conf, which is
// what an unqualified name would be expanded with.
func (f *fakeDNS) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if !strings.HasPrefix(network, "udp") {
				return nil, errors.New("fake DNS serves UDP only")
			}
			var d net.Dialer
			return d.DialContext(ctx, "udp", f.conn.LocalAddr().String())
		},
	}
}

func TestBMCHostnameIsNotExpandedWithTheSearchList(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ address, asked string }{
		{"https://bmc1.lab:8443", "bmc1.lab."},
		{"https://bmc1", "bmc1."},
	} {
		t.Run(tc.address, func(t *testing.T) {
			t.Parallel()
			dns := startFakeDNS(t)
			dialer := &net.Dialer{Resolver: dns.resolver()}
			endpoint, err := url.Parse(tc.address)
			if err != nil {
				t.Fatal(err)
			}
			c, err := newHTTPClient(endpoint, false, nil, dialer.DialContext, nil)
			if err != nil {
				t.Fatalf("newHTTPClient: %v", err)
			}
			_, err = getThrough(t, c, tc.address+"/redfish/v1/")
			var dnsErr *net.DNSError
			if !errors.As(err, &dnsErr) {
				t.Fatalf("expected the lookup to fail, got %v", err)
			}
			asked := dns.names()
			if len(asked) == 0 {
				t.Fatal("the resolver never asked the fake DNS server")
			}
			for _, name := range asked {
				if name != tc.asked {
					t.Fatalf("the resolver asked for %q (all: %q); only %q may be looked up", name, asked, tc.asked)
				}
			}
		})
	}
}
