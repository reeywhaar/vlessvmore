package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vlessvmore/internal/config"
)

// corsServer is testServer with cors_origins set.
func corsServer(t *testing.T, origins string) *Server {
	t.Helper()
	s, _ := testServer(t)
	cfg, err := config.Parse(strings.NewReader(`{"host":"vpn.example.test","cors_origins":` + origins + `}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	s.cfg = cfg
	return s
}

func preflight(t *testing.T, s *Server, path, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("OPTIONS", path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(rec, req)
	return rec
}

// Nothing may change for a deployment that has not asked for CORS.
func TestCORSOffByDefault(t *testing.T) {
	s, _ := testServer(t)

	rec := preflight(t, s, "/api/users", "https://dash.example.com")
	if rec.Code != http.StatusNotFound {
		t.Errorf("preflight = %d, want 404", rec.Code)
	}
	if v := rec.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("Access-Control-Allow-Origin = %q on a server with no cors_origins", v)
	}
}

func TestCORSAllowsListedOrigin(t *testing.T) {
	s := corsServer(t, `["https://dash.example.com"]`)

	rec := preflight(t, s, "/api/users", "https://dash.example.com")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	h := rec.Header()
	if got := h.Get("Access-Control-Allow-Origin"); got != "https://dash.example.com" {
		t.Errorf("Allow-Origin = %q", got)
	}
	// Without Authorization in Allow-Headers the browser drops the real request, and the
	// dashboard fails with no useful error.
	if got := h.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Errorf("Allow-Headers = %q, must permit Authorization", got)
	}
	if got := h.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "PATCH") {
		t.Errorf("Allow-Methods = %q, must cover the verbs the API uses", got)
	}
	// Echoing one origin makes the response origin-dependent; a cache has to know that.
	if got := h.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

// The whole reason the Origin is the gate: an origin nobody configured must not be able
// to tell /api/users from /nonsense.
func TestCORSRefusalIsIndistinguishable(t *testing.T) {
	s := corsServer(t, `["https://dash.example.com"]`)

	real := preflight(t, s, "/api/users", "https://evil.example")
	fake := preflight(t, s, "/nonsense", "https://evil.example")

	if real.Code != http.StatusNotFound {
		t.Fatalf("preflight from an unlisted origin = %d, want 404", real.Code)
	}
	if real.Body.String() != fake.Body.String() {
		t.Errorf("a real path answers %q, an unmatched one %q", real.Body.String(), fake.Body.String())
	}
	if got := headerFingerprint(real.Result().Header); got != headerFingerprint(fake.Result().Header) {
		t.Errorf("headers differ between a real and an unmatched path:\n%s\n---\n%s",
			got, headerFingerprint(fake.Result().Header))
	}
}

// A request with no Origin at all is the CLI, curl, or a subscription client. It must be
// untouched, including the refusal path.
func TestCORSIgnoresRequestsWithoutOrigin(t *testing.T) {
	s := corsServer(t, `["https://dash.example.com"]`)

	rec := preflight(t, s, "/api/users", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("preflight with no Origin = %d, want 404", rec.Code)
	}
	if v := rec.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("Allow-Origin = %q on a request that sent no Origin", v)
	}
}

func TestCORSWildcard(t *testing.T) {
	s := corsServer(t, `["*"]`)

	rec := preflight(t, s, "/api/users", "https://anyone.example")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	// "*" literally, not the caller's origin: cacheable, and one response is enough to
	// see the deployment is open.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want *", got)
	}
}

// The header goes on the real response too, or the browser discards the body it just
// fetched.
func TestCORSTagsAuthenticatedResponses(t *testing.T) {
	s := corsServer(t, `["https://dash.example.com"]`)
	tok := mintToken(t, s.store)

	req := httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Origin", "https://dash.example.com")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://dash.example.com" {
		t.Errorf("Allow-Origin = %q on the real response", got)
	}
}

// An allowed origin still needs a token: CORS decides who may ask, not who may in.
func TestCORSDoesNotBypassAuth(t *testing.T) {
	s := corsServer(t, `["*"]`)

	req := httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Origin", "https://anyone.example")
	rec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — an allowed origin with no token is still refused", rec.Code)
	}
}

func TestCORSOriginMatchingIsLenient(t *testing.T) {
	s := corsServer(t, `["https://Dash.Example.com/"]`)

	for _, origin := range []string{
		"https://dash.example.com",
		"https://DASH.EXAMPLE.COM",
	} {
		t.Run(origin, func(t *testing.T) {
			if rec := preflight(t, s, "/api/users", origin); rec.Code != http.StatusNoContent {
				t.Errorf("preflight = %d, want 204", rec.Code)
			}
		})
	}

	// A different host, and a scheme downgrade, are different origins.
	for _, origin := range []string{
		"http://dash.example.com",
		"https://dash.example.com.evil.test",
		"https://dash.example.com:8443",
	} {
		t.Run("refuses "+origin, func(t *testing.T) {
			if rec := preflight(t, s, "/api/users", origin); rec.Code != http.StatusNotFound {
				t.Errorf("preflight = %d, want 404", rec.Code)
			}
		})
	}
}

// Every one of these is the same origin as far as a browser is concerned, and each is
// something an operator might plausibly type.
func TestCORSDefaultPortsMatch(t *testing.T) {
	tests := map[string]string{
		// written in config          // what the browser sends
		"https://dash.example.com":     "https://dash.example.com",
		"https://dash.example.com:443": "https://dash.example.com",
		"https://dash.example.com/":    "https://dash.example.com",
		"http://localhost:80":          "http://localhost",
		"http://localhost:5173":        "http://localhost:5173",
	}
	for configured, sent := range tests {
		t.Run(configured, func(t *testing.T) {
			s := corsServer(t, `["`+configured+`"]`)
			if rec := preflight(t, s, "/api/users", sent); rec.Code != http.StatusNoContent {
				t.Errorf("configured %q, browser sent %q: preflight = %d, want 204",
					configured, sent, rec.Code)
			}
		})
	}

	// A non-default port is part of the origin and must not be stripped.
	s := corsServer(t, `["https://dash.example.com:8443"]`)
	if rec := preflight(t, s, "/api/users", "https://dash.example.com"); rec.Code != http.StatusNotFound {
		t.Errorf("preflight = %d: a non-default port must still be required", rec.Code)
	}
}

func TestCORSOriginsValidation(t *testing.T) {
	tests := map[string]bool{
		`[]`:                               true,
		`["*"]`:                            true,
		`["https://dash.example.com"]`:     true,
		`["http://localhost:5173"]`:        true,
		`["https://a.test","*"]`:           true,
		`["dash.example.com"]`:             false, // no scheme
		`["https://dash.example.com/app"]`: false, // a path is never part of an Origin
		`["https://dash.example.com?x=1"]`: false,
		`["https://"]`:                     false,
	}
	for origins, wantOK := range tests {
		t.Run(origins, func(t *testing.T) {
			_, err := config.Parse(strings.NewReader(`{"host":"h.test","cors_origins":` + origins + `}`))
			if wantOK && err != nil {
				t.Errorf("rejected a valid list: %v", err)
			}
			if !wantOK && err == nil {
				t.Error("accepted an origin a browser will never send")
			}
		})
	}
}
