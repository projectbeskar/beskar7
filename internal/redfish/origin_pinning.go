package redfish

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// originPinnedTransport sends a request only to the origin (scheme, host and
// port) of the BMC address the credentials were authorised for, and refuses
// everything else before it is dialed (SEC-17c). The bmc-addresses annotation
// (D-030) is checked against that address once, before the client exists, so
// nothing may move a request off it afterwards. Two things otherwise would:
//
//   - gofish builds each request URL by appending a path the BMC returned
//     (@odata.id) to the address as text, so an @odata.id of
//     "@attacker:8443/redfish/v1/Systems" turns the address into userinfo and
//     names a new host;
//   - net/http follows redirects, and keeps the Authorization header gofish
//     set when the redirect stays on the same hostname or goes to a subdomain
//     of it, whatever the port or scheme, so a redirect to http:// on the same
//     host sends the credentials in plaintext.
//
// It sits under the http.Client, so it sees every redirect hop as well.
type originPinnedTransport struct {
	base   http.RoundTripper
	scheme string
	host   string
	port   string
}

// pinToOrigin wraps base so that it serves endpoint's origin only.
func pinToOrigin(base http.RoundTripper, endpoint *url.URL) (*originPinnedTransport, error) {
	scheme := strings.ToLower(endpoint.Scheme)
	port := originPort(scheme, endpoint.Port())
	host := strings.ToLower(endpoint.Hostname())
	if host == "" || port == "" {
		return nil, errors.New("redfish: the BMC address needs an http or https scheme and a host")
	}
	return &originPinnedTransport{base: base, scheme: scheme, host: host, port: port}, nil
}

// originPort returns port, or the default port for scheme when it is empty.
// It returns "" for a scheme the transport does not serve.
func originPort(scheme, port string) string {
	if port != "" {
		return port
	}
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

func (t *originPinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.serves(req.URL) {
		// A RoundTripper closes the body even when it refuses the request.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		// Host never carries userinfo, so neither does this message.
		return nil, fmt.Errorf("redfish: refused a request to %s://%s: it is not the BMC's address %s://%s",
			req.URL.Scheme, req.URL.Host, t.scheme, net.JoinHostPort(t.host, t.port))
	}
	return t.base.RoundTrip(req)
}

func (t *originPinnedTransport) serves(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == t.scheme &&
		strings.ToLower(u.Hostname()) == t.host &&
		originPort(scheme, u.Port()) == t.port
}

// CloseIdleConnections lets gofish's Logout reach the base transport through
// http.Client.CloseIdleConnections.
func (t *originPinnedTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
