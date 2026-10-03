package redfish

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type dialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// systemDial is the dialer every BMC connection is made with outside tests.
var systemDial dialContextFunc = http.DefaultTransport.(*http.Transport).DialContext

// dialAbsolute returns a dialer that resolves bmcHost as an absolute DNS name,
// by dialing it with a trailing dot, and dials every other address unchanged
// (D-032, SEC-17a).
//
// The bmc-addresses annotation authorises a name as written, but the pod's
// resolver expands a name it does not consider absolute with its search list
// first (ndots:5 in a Kubernetes pod), so bmc1.lab would be tried as
// bmc1.lab.<namespace>.svc.cluster.local, and whoever can create a Service
// named bmc1 in a namespace called lab would receive the connection and the
// credentials. Only the dial changes: the request's Host header and the name
// the BMC's certificate is checked against stay as written.
//
// Addresses other than bmcHost are left alone because the only one the
// transport dials is the --bmc-proxy proxy (D-035), which the operator names,
// and which then resolves bmcHost itself: D-032 cannot reach a proxied
// connection, and the docs say so. An IP literal has nothing to resolve.
func dialAbsolute(bmcHost string, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return dial(ctx, network, absoluteDialAddress(bmcHost, address))
	}
}

func absoluteDialAddress(bmcHost, address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !strings.EqualFold(host, bmcHost) || strings.HasSuffix(host, ".") {
		return address
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return address
	}
	return net.JoinHostPort(host+".", port)
}
