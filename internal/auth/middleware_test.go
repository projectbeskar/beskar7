/*
Copyright 2024 The Beskar7 Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

// recordingHandler counts invocations and records the last token (when set on
// the request via context). Used to prove that next is/isn't called.
type recordingHandler struct {
	called int
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.called++
	w.WriteHeader(http.StatusOK)
}

// fixedVerifier returns a predetermined error and records the token it was called
// with. If err is nil, RequireBearer must invoke next.
type fixedVerifier struct {
	err         error
	calls       int
	tokenSeen   string
	requestSeen *http.Request
}

func (v *fixedVerifier) Verify(token string, r *http.Request) error {
	v.calls++
	v.tokenSeen = token
	v.requestSeen = r
	return v.err
}

func newRequest(t *testing.T, authHeader string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// readBody fully reads the response body and returns it as a string.
func readBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func TestRequireBearer_MissingHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	v := &fixedVerifier{err: nil}
	next := &recordingHandler{}

	RequireBearer(logr.Discard(), v.Verify, next).ServeHTTP(rec, newRequest(t, ""))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if v.calls != 0 {
		t.Errorf("verifier called %d times, want 0 (header missing)", v.calls)
	}
	if next.called != 0 {
		t.Errorf("next called %d times, want 0", next.called)
	}
	body := readBody(t, rec)
	if !strings.Contains(body, `"unauthorized"`) {
		t.Errorf("body = %q, want generic unauthorized JSON", body)
	}
}

func TestRequireBearer_MalformedHeader_BasicScheme(t *testing.T) {
	rec := httptest.NewRecorder()
	v := &fixedVerifier{}
	next := &recordingHandler{}

	RequireBearer(logr.Discard(), v.Verify, next).ServeHTTP(rec, newRequest(t, "Basic dXNlcjpwYXNz"))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if v.calls != 0 {
		t.Errorf("verifier called %d times, want 0 (Basic scheme rejected)", v.calls)
	}
	if next.called != 0 {
		t.Errorf("next called %d times, want 0", next.called)
	}
}

func TestRequireBearer_MalformedHeader_BearerWithoutValue(t *testing.T) {
	rec := httptest.NewRecorder()
	v := &fixedVerifier{}
	next := &recordingHandler{}

	RequireBearer(logr.Discard(), v.Verify, next).ServeHTTP(rec, newRequest(t, "Bearer "))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if v.calls != 0 {
		t.Errorf("verifier called %d times, want 0 (empty token)", v.calls)
	}
}

func TestRequireBearer_MalformedHeader_BearerNoSpace(t *testing.T) {
	rec := httptest.NewRecorder()
	v := &fixedVerifier{}
	next := &recordingHandler{}

	RequireBearer(logr.Discard(), v.Verify, next).ServeHTTP(rec, newRequest(t, "Bearertoken"))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if v.calls != 0 {
		t.Errorf("verifier called %d times, want 0 (no space)", v.calls)
	}
}

func TestRequireBearer_MalformedHeader_TokenWithWhitespace(t *testing.T) {
	rec := httptest.NewRecorder()
	v := &fixedVerifier{}
	next := &recordingHandler{}

	RequireBearer(logr.Discard(), v.Verify, next).ServeHTTP(rec, newRequest(t, "Bearer my token"))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if v.calls != 0 {
		t.Errorf("verifier called %d times, want 0 (token with spaces rejected)", v.calls)
	}
}

func TestRequireBearer_VerifierError_ReturnsOpaque401(t *testing.T) {
	rec := httptest.NewRecorder()
	v := &fixedVerifier{err: errors.New("token mismatch for host default/foo")}
	next := &recordingHandler{}

	RequireBearer(logr.Discard(), v.Verify, next).ServeHTTP(rec, newRequest(t, "Bearer abc123"))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if v.calls != 1 {
		t.Errorf("verifier called %d times, want 1", v.calls)
	}
	if v.tokenSeen != "abc123" {
		t.Errorf("verifier saw token %q, want %q", v.tokenSeen, "abc123")
	}
	if next.called != 0 {
		t.Errorf("next called %d times, want 0 (verifier rejected)", next.called)
	}
	// Body must NOT echo the verifier's error message — that would leak which
	// host the operator was probing or whether the host exists.
	body := readBody(t, rec)
	if strings.Contains(body, "token mismatch") || strings.Contains(body, "default/foo") {
		t.Errorf("body leaked verifier error detail: %q", body)
	}
}

func TestRequireBearer_VerifierSuccess_InvokesNext(t *testing.T) {
	rec := httptest.NewRecorder()
	v := &fixedVerifier{err: nil}
	next := &recordingHandler{}

	RequireBearer(logr.Discard(), v.Verify, next).ServeHTTP(rec, newRequest(t, "Bearer good-token"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if v.calls != 1 {
		t.Errorf("verifier called %d times, want 1", v.calls)
	}
	if next.called != 1 {
		t.Errorf("next called %d times, want 1", next.called)
	}
	if v.tokenSeen != "good-token" {
		t.Errorf("verifier saw token %q, want %q", v.tokenSeen, "good-token")
	}
}

func TestRequireBearer_CaseInsensitiveScheme(t *testing.T) {
	rec := httptest.NewRecorder()
	v := &fixedVerifier{err: nil}
	next := &recordingHandler{}

	// RFC 7235 §2.1: scheme is case-insensitive.
	RequireBearer(logr.Discard(), v.Verify, next).ServeHTTP(rec, newRequest(t, "bearer good-token"))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (lowercase 'bearer' must be accepted)", rec.Code)
	}
	if next.called != 1 {
		t.Errorf("next called %d times, want 1", next.called)
	}
}

func TestRequireBearer_NilVerifier_FailClosed(t *testing.T) {
	rec := httptest.NewRecorder()
	next := &recordingHandler{}

	// A programming error must not silently produce an open handler.
	RequireBearer(logr.Discard(), nil, next).ServeHTTP(rec, newRequest(t, "Bearer something"))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (nil verifier must fail closed)", rec.Code)
	}
	if next.called != 0 {
		t.Errorf("next called %d times, want 0", next.called)
	}
}

func TestExtractBearerToken_AllRejections(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantOK  bool
		wantTok string
	}{
		{"empty", "", false, ""},
		{"too short", "Bear", false, ""},
		{"wrong scheme", "Basic abc", false, ""},
		{"bearer no space", "Bearertoken", false, ""},
		{"bearer empty token", "Bearer ", false, ""},
		{"bearer empty after trim", "Bearer    ", false, ""},
		{"bearer token with internal space", "Bearer foo bar", false, ""},
		{"bearer token with tab", "Bearer foo\tbar", false, ""},
		{"valid", "Bearer abc123", true, "abc123"},
		{"valid lowercase", "bearer abc123", true, "abc123"},
		{"valid leading whitespace", "Bearer   abc123", true, "abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, ok := extractBearerToken(tc.input)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (input=%q)", ok, tc.wantOK, tc.input)
			}
			if tok != tc.wantTok {
				t.Errorf("token = %q, want %q (input=%q)", tok, tc.wantTok, tc.input)
			}
		})
	}
}
