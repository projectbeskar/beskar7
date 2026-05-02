package redfish

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

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
	c := newHTTPClient(false)
	if c.Timeout != defaultHTTPTimeout {
		t.Fatalf("expected Timeout=%v, got %v", defaultHTTPTimeout, c.Timeout)
	}
}

func TestNewHTTPClient_InsecureFalse(t *testing.T) {
	t.Parallel()
	c := newHTTPClient(false)
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=false")
	}
}

func TestNewHTTPClient_InsecureTrue(t *testing.T) {
	t.Parallel()
	c := newHTTPClient(true)
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=true")
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
