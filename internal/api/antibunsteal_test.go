package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// Dozens of tests make a refusal; paying the real floor for each would add seconds to
// `go test`. TestRefusalsArePadded puts it back.
func TestMain(m *testing.M) {
	antibunstealFloor, antibunstealJitter = 0, 0
	os.Exit(m.Run())
}

func withPadding(t *testing.T, floor, jit time.Duration) {
	t.Helper()
	oldFloor, oldJitter := antibunstealFloor, antibunstealJitter
	antibunstealFloor, antibunstealJitter = floor, jit
	t.Cleanup(func() { antibunstealFloor, antibunstealJitter = oldFloor, oldJitter })
}

// Each of these is a different reason to say no, and none of them may be tellable apart.
func TestRefusalsAreIndistinguishable(t *testing.T) {
	s, st := testServer(t)
	h := s.Handler(true)

	// A real user, so the "unknown subscription token" case is compared against a store
	// that actually has something to find.
	newUserWithSub(t, s, `{"name":"alice"}`)
	good := mintToken(t, st)

	cases := []struct {
		name   string
		method string
		path   string
		header string
	}{
		{"unmatched path", "GET", "/nonsense", ""},
		{"unmatched path under api", "GET", "/api/nonexistent", "Bearer " + good},
		{"no token", "GET", "/api/users", ""},
		{"bad token", "GET", "/api/users", "Bearer nope"},
		{"wrong scheme", "GET", "/api/users", "Basic abc"},
		{"wrong method on a real path", "POST", "/api/server", "Bearer " + good},
		{"unknown subscription token", "GET", SubPath + "NOSUCHTOKEN", ""},
		{"healthz, which is socket-only", "GET", "/healthz", ""},
		{"deep path", "GET", "/a/b/c/d", ""},
	}

	type response struct {
		status  int
		body    string
		headers string
	}
	var first response
	var firstName string

	for i, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		got := response{
			status:  rec.Code,
			body:    rec.Body.String(),
			headers: headerFingerprint(rec.Result().Header),
		}
		if got.status != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", tc.name, got.status)
		}
		if i == 0 {
			first, firstName = got, tc.name
			continue
		}
		if got.body != first.body {
			t.Errorf("%s: body %q differs from %s: %q", tc.name, got.body, firstName, first.body)
		}
		if got.headers != first.headers {
			t.Errorf("%s: headers %q differ from %s: %q", tc.name, got.headers, firstName, first.headers)
		}
	}
}

// headerFingerprint renders the headers a client can actually compare. Date is dropped
// because it moves on its own, and Content-Length is implied by the body we already
// compare.
func headerFingerprint(h http.Header) string {
	var parts []string
	for k, v := range h {
		if k == "Date" {
			continue
		}
		parts = append(parts, k+": "+strings.Join(v, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// WWW-Authenticate would name the software in its realm, and admit there is something to
// authenticate against at all.
func TestNoAuthenticateHeaderIsEverSent(t *testing.T) {
	s, st := testServer(t)
	h := s.Handler(true)

	for _, header := range []string{"", "Bearer nope", "Bearer " + mintToken(t, st)} {
		req := httptest.NewRequest("GET", "/api/users", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if v := rec.Result().Header.Get("WWW-Authenticate"); v != "" {
			t.Errorf("Authorization=%q got WWW-Authenticate: %q", header, v)
		}
	}
}

// Byte-identical to what an unrouted stdlib mux produces: nothing custom to compare
// against a known Go server.
func TestRefusalBodyIsStdlibNotFound(t *testing.T) {
	s, _ := testServer(t)

	rec := httptest.NewRecorder()
	http.NotFound(rec, httptest.NewRequest("GET", "/whatever", nil))
	want := rec.Body.String()

	req := httptest.NewRequest("GET", "/api/users", nil)
	got := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(got, req)
	if got.Body.String() != want {
		t.Errorf("refusal body = %q, want stdlib %q", got.Body.String(), want)
	}
}

// Without padding, "this subscription token exists" takes measurably longer to refuse
// than "this path was never registered", and enough samples recover the difference.
func TestRefusalsArePadded(t *testing.T) {
	withPadding(t, 40*time.Millisecond, 20*time.Millisecond)

	s, _ := testServer(t)

	start := time.Now()
	req := httptest.NewRequest("GET", "/api/users", nil)
	rec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if elapsed < antibunstealFloor {
		t.Errorf("public refusal took %v, want at least the floor %v", elapsed, antibunstealFloor)
	}
	if elapsed > antibunstealFloor+antibunstealJitter+2*time.Second {
		t.Errorf("public refusal took %v, far beyond floor+jitter", elapsed)
	}
}

// Reaching the socket already means root in the container, and the CLI hits it constantly.
func TestSocketRefusalsAreNotPadded(t *testing.T) {
	withPadding(t, 500*time.Millisecond, 0)

	s, _ := testServer(t)

	start := time.Now()
	rec := do(t, s, "GET", "/nonsense", "")
	elapsed := time.Since(start)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if elapsed >= antibunstealFloor {
		t.Errorf("socket refusal took %v; it should not be padded", elapsed)
	}
}

func TestIsPublic(t *testing.T) {
	tests := map[string]bool{
		"/":                  true,
		SubPath + "abc":      true,
		SubPath:              true,
		"/healthz":           false,
		"/api/users":         false,
		"/api":               false,
		"/subscription":      false,
		"/nonsense":          false,
		"/sub":               false,
		SubPath + "a/../../": true, // Already cleaned by net/http before we see it.
	}
	for path, want := range tests {
		if got := isPublic(path); got != want {
			t.Errorf("isPublic(%q) = %v, want %v", path, got, want)
		}
	}
}
