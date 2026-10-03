package redfish

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/url"
	"time"
)

// bmcProxy is the one proxy every connection to a BMC goes through (D-035).
// A nil *bmcProxy means a direct connection, which is the default.
//
// The environment is never consulted. http.ProxyFromEnvironment is what Go's
// default transport uses, and it let an operator's HTTPS_PROXY decide how
// credentials travel to a BMC and who resolves the BMC's name (defeating
// D-032), without anything on the PhysicalHost saying so. The only way to
// proxy BMC traffic is the manager's --bmc-proxy flag.
type bmcProxy struct {
	url *url.URL

	// roots verifies the certificate of an https:// proxy. Nil means the
	// system roots. It is a field only so a test can trust its own proxy.
	roots *x509.CertPool
}

// newBMCProxy returns the bmcProxy for u, or nil for a nil u. It copies u so
// the caller's URL can change afterwards without changing where credentials go.
func newBMCProxy(u *url.URL) *bmcProxy {
	if u == nil {
		return nil
	}
	c := *u
	return &bmcProxy{url: &c}
}

// proxyFunc is the http.Transport.Proxy for a BMC transport. A nil Proxy is
// how Go says "connect directly", so a direct transport has no proxy function
// at all, and a proxied one sends every request, loopback included, to the
// one proxy: unlike http.ProxyFromEnvironment it has no NO_PROXY and no
// loopback exemption.
func (p *bmcProxy) proxyFunc() func(*http.Request) (*url.URL, error) {
	if p == nil {
		return nil
	}
	return http.ProxyURL(p.url)
}

// proxyTLSTimeout bounds the TLS handshake with an https:// proxy, as
// TLSHandshakeTimeout does for the BMC itself, which Go ignores when the
// transport has its own TLS dialer.
const proxyTLSTimeout = 10 * time.Second

// tlsDialer returns the dialer for the TLS connection to the proxy, or nil
// when the proxy is not an https:// one and Go's own dial is right.
//
// A transport has one TLS configuration, and Go applies it to the hop to an
// https:// proxy as well as to the BMC behind it. That would verify the proxy
// with the BMC's CA bundle, which does not know it, and skip verifying it
// whenever the BMC's host has insecureSkipVerify: true, which is an opt-in
// about the BMC. So the proxy hop gets its own configuration: the system
// roots, verified against the proxy's name, whatever is configured for the
// BMC. Go still runs the BMC's TLS inside the CONNECT tunnel with the BMC's
// configuration.
func (p *bmcProxy) tlsDialer(dial dialContextFunc) dialContextFunc {
	if p == nil || p.url.Scheme != "https" {
		return nil
	}
	cfg := &tls.Config{
		ServerName: p.url.Hostname(),
		RootCAs:    p.roots,
		MinVersion: tls.VersionTLS12,
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, proxyTLSTimeout)
		defer cancel()
		raw, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		conn := tls.Client(raw, cfg.Clone())
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return conn, nil
	}
}

// NewClientFactory returns a RedfishClientFactory whose clients reach every BMC
// through proxy, the manager's --bmc-proxy (D-035). A nil proxy returns
// NewClient, which connects directly. Neither reads the environment.
//
// Only the connection changes. The BMC's address is still checked against the
// credentials Secret's bmc-addresses annotation before a client is built
// (D-030), and a request to any origin but the BMC's is still refused before
// it reaches the proxy (SEC-17c). The proxy resolves the BMC's name, so D-032's
// absolute-name dialing cannot apply to a proxied connection.
func NewClientFactory(proxy *url.URL) RedfishClientFactory {
	if proxy == nil {
		return NewClient
	}
	p := newBMCProxy(proxy)
	return func(ctx context.Context, address, username, password string, insecure bool, caBundle []byte) (Client, error) {
		return newClient(ctx, systemDial, p, address, username, password, insecure, caBundle)
	}
}
