package redfish

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/stmcginnis/gofish/common"
)

// timeoutError mimics the error http.Client returns when its Timeout elapses:
// not a *net.OpError, but a net.Error whose Timeout() is true.
type timeoutError struct{}

func (timeoutError) Error() string {
	return "net/http: request canceled (Client.Timeout exceeded while awaiting headers)"
}
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func dialError(errno syscall.Errno) error {
	return &url.Error{
		Op:  "Get",
		URL: "https://bmc.example.invalid/redfish/v1/",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)},
	}
}

func TestIsTransientConnectionError(t *testing.T) {
	refusedInCollection := common.NewCollectionError()
	refusedInCollection.Failures["/redfish/v1/Systems"] = dialError(syscall.ECONNREFUSED)

	mixedCollection := common.NewCollectionError()
	mixedCollection.Failures["/redfish/v1/Systems/2"] = common.ConstructError(401, []byte("nope"))
	mixedCollection.Failures["/redfish/v1/Systems/1"] = dialError(syscall.ECONNREFUSED)

	authInCollection := common.NewCollectionError()
	authInCollection.Failures["/redfish/v1/Systems/1"] = common.ConstructError(401, []byte("nope"))

	cases := []struct {
		name      string
		err       error
		transient bool
		desc      string
	}{
		{"nil", nil, false, ""},
		{"connection refused, wrapped like NewClient does",
			fmt.Errorf("failed to connect to Redfish endpoint https://bmc: %w", dialError(syscall.ECONNREFUSED)),
			true, "connection refused"},
		{"connection reset while reading",
			&url.Error{Op: "Get", URL: "https://bmc/redfish/v1/", Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}},
			true, "connection reset"},
		{"no route to host", dialError(syscall.EHOSTUNREACH), true, "host unreachable"},
		{"network unreachable", dialError(syscall.ENETUNREACH), true, "host unreachable"},
		{"other dial errno falls into the generic network bucket", dialError(syscall.EADDRNOTAVAIL), true, "network error"},
		{"DNS lookup failure inside the dial",
			&url.Error{Op: "Get", URL: "https://bmc/redfish/v1/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "bmc", IsNotFound: true}}},
			true, "DNS lookup failed"},
		{"http.Client timeout", &url.Error{Op: "Get", URL: "https://bmc/redfish/v1/", Err: timeoutError{}}, true, "timed out"},
		{"reconcile context deadline (doWithCtx)", context.DeadlineExceeded, true, "timed out"},
		{"connection closed mid-response", &url.Error{Op: "Get", URL: "https://bmc/redfish/v1/", Err: io.EOF}, true, "connection closed"},
		{"BMC still starting (503)", common.ConstructError(503, []byte("starting")), true, "HTTP 503"},
		{"collection walk hit a refused connection, wrapped like getSystemService does",
			fmt.Errorf("failed to retrieve systems: %w", refusedInCollection), true, "connection refused"},
		{"collection walk with one refused and one 401 item (deterministic pick by link)",
			mixedCollection, true, "connection refused"},

		{"rejected credentials", common.ConstructError(401, []byte("nope")), false, ""},
		{"server error (500)", common.ConstructError(500, []byte("boom")), false, ""},
		{"collection walk with only auth failures", authInCollection, false, ""},
		{"certificate rejected by the client",
			&url.Error{Op: "Get", URL: "https://bmc/redfish/v1/", Err: x509.UnknownAuthorityError{}},
			false, ""},
		{"context canceled (manager shutting down)", context.Canceled, false, ""},
		{"plain error", errors.New("no systems found"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientConnectionError(tc.err); got != tc.transient {
				t.Fatalf("IsTransientConnectionError(%v) = %v, want %v", tc.err, got, tc.transient)
			}
			if got := DescribeTransientConnectionError(tc.err); got != tc.desc {
				t.Fatalf("DescribeTransientConnectionError(%v) = %q, want %q", tc.err, got, tc.desc)
			}
		})
	}
}
