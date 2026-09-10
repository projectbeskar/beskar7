package redfish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"syscall"

	"github.com/stmcginnis/gofish/common"
)

// IsTransientConnectionError reports whether err is a failure to reach the BMC
// at the network or transport level — the kind that clears on its own once the
// BMC, or the path to it, is back: connection refused or reset, no route, a
// DNS lookup that fails, a dial/TLS/request timeout, a connection closed
// mid-response, or an HTTP 502/503/504 from a BMC that is still starting up.
//
// Everything else is not transient: a malformed address, a TLS certificate the
// client rejects, rejected credentials (401/403), a Redfish tree without a
// ComputerSystem. Those need a spec, Secret or firmware change, and retrying
// them on a short timer would only add noise.
//
// gofish reports failures inside a collection walk as a *common.CollectionError
// whose Error() text JSON-encodes the per-item errors; it does not implement
// Unwrap, so the per-item errors are inspected explicitly.
func IsTransientConnectionError(err error) bool {
	_, ok := classifyTransient(err)
	return ok
}

// DescribeTransientConnectionError returns a short, stable description of a
// transient failure ("connection refused", "timed out", ...) for a status
// message. It is deliberately not the raw error text: that text names the URL
// that failed and differs from one attempt to the next, and a status message
// that changes on every attempt is a watch event that triggers the next
// attempt at once. Returns "" when err is not transient.
func DescribeTransientConnectionError(err error) string {
	desc, _ := classifyTransient(err)
	return desc
}

func classifyTransient(err error) (string, bool) {
	if err == nil {
		return "", false
	}

	var collErr *common.CollectionError
	if errors.As(err, &collErr) {
		links := make([]string, 0, len(collErr.Failures))
		for link := range collErr.Failures {
			links = append(links, link)
		}
		// Map order is random; keep the description deterministic.
		sort.Strings(links)
		for _, link := range links {
			if desc, ok := classifyTransient(collErr.Failures[link]); ok {
				return desc, true
			}
		}
		return "", false
	}

	// A resolver failure surfaces inside the dial *net.OpError; classify it
	// before the generic checks below so it keeps its own description.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS lookup failed", true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out", true
	}
	// *url.Error, *net.OpError and the http.Client timeout all implement
	// net.Error; only a real timeout is transient on this path (a *url.Error
	// around an x509 failure reports Timeout()=false and is not).
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out", true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED:
			return "connection refused", true
		case syscall.ECONNRESET, syscall.EPIPE:
			return "connection reset", true
		case syscall.EHOSTUNREACH, syscall.ENETUNREACH:
			return "host unreachable", true
		}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// A dial/read/write failure with an errno not named above.
		return "network error", true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection closed", true
	}
	var httpErr *common.Error
	if errors.As(err, &httpErr) {
		switch httpErr.HTTPReturnedStatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return fmt.Sprintf("HTTP %d", httpErr.HTTPReturnedStatusCode), true
		}
	}
	return "", false
}
