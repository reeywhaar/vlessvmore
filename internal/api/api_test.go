package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vlessvmore/internal/config"
	"vlessvmore/internal/singbox"
	"vlessvmore/internal/store"
)

// RFC 7748 Alice, so the expected public key below is a published value.
const (
	testPrivateKey = "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCo"
	testPublicKey  = "hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo"
)

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	const raw = `{
  "host": "vpn.example.test"
}`
	cfg, err := config.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// A known identity, so link and server-info assertions have a fixed public key.
	if err := st.Identity.Replace(store.Identity{
		PrivateKey: testPrivateKey,
		ShortID:    "0102030405060708",
	}); err != nil {
		t.Fatalf("install test identity: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := singbox.NewManager(cfg, st, log)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return New(cfg, st, mgr, log), st
}

// mintToken creates a real API token and returns its secret. There is no bootstrap
// credential to shortcut with, so tests authenticate exactly the way callers do.
func mintToken(t *testing.T, st *store.Store) string {
	t.Helper()
	_, secret, err := st.Tokens.Create("test", time.Now())
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return secret
}

// do issues a request against the trusted (socket) handler, which skips auth.
func do(t *testing.T, s *Server, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	rec := httptest.NewRecorder()
	s.Handler(false).ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response: %v\n%s", err, rec.Body.String())
	}
	return out
}

// A failed authentication is a 404, not a 401: see the authenticate doc comment. The
// interesting half of that promise — that it is the *same* 404 as any other refusal — is
// tested in probe_test.go.
func TestAuthRequiredOnTCP(t *testing.T) {
	s, st := testServer(t)
	h := s.Handler(true)
	token := mintToken(t, st)

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusNotFound},
		{"wrong scheme", "Basic abc", http.StatusNotFound},
		{"empty bearer", "Bearer ", http.StatusNotFound},
		{"wrong token", "Bearer nope", http.StatusNotFound},
		{"minted token", "Bearer " + token, http.StatusOK},
		{"case-insensitive scheme", "bearer " + token, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/users", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

// The socket is trusted: reaching it already requires root in the container.
func TestSocketHandlerSkipsAuth(t *testing.T) {
	s, _ := testServer(t)
	rec := do(t, s, "GET", "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestRootIsUnauthenticated(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET / = %d, want 200", rec.Code)
	}
}

// The health check exists for the container runtime, which reaches it over the socket.
// Exposing it on the public listener would answer strangers with JSON no static site
// serves, which is exactly the fingerprint the cover page is there to avoid.
func TestHealthzIsSocketOnly(t *testing.T) {
	s, _ := testServer(t)

	if rec := do(t, s, "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("socket GET /healthz = %d, want 200", rec.Code)
	}

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("public GET /healthz = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// The root handler is a catch-all only for the exact root. An unknown path must not
// come back 200 just because it went unmatched.
func TestUnknownPathIsNotCoveredByRoot(t *testing.T) {
	s, st := testServer(t)
	h := s.Handler(true)

	req := httptest.NewRequest("GET", "/api/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer "+mintToken(t, st))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("unknown path returned 200; the root handler is matching too broadly")
	}
}

func TestRootLeaksNothing(t *testing.T) {
	s, _ := testServer(t)
	rec := do(t, s, "GET", "/", "")
	body := rec.Body.String()
	for _, forbidden := range []string{"vlessvmore", "sing-box", "reality", "vless"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("cover page mentions %q, which defeats the point: %s", forbidden, body)
		}
	}
}

func TestCreateAndGetUser(t *testing.T) {
	s, _ := testServer(t)

	rec := do(t, s, "POST", "/api/users", `{"name":"alice","quota_bytes":1024}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeBody[struct {
		Result   UserResponse `json:"result"`
		Reloaded bool         `json:"reloaded"`
	}](t, rec)
	if created.Result.Name != "alice" {
		t.Errorf("name = %q", created.Result.Name)
	}
	if created.Result.UUID == "" {
		t.Error("uuid should be generated")
	}

	// Resolvable by id and by name.
	for _, ref := range []string{created.Result.ID, "alice"} {
		rec := do(t, s, "GET", "/api/users/"+ref, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET user %s = %d: %s", ref, rec.Code, rec.Body.String())
		}
		got := decodeBody[UserResponse](t, rec)
		if got.ID != created.Result.ID {
			t.Errorf("GET %s returned %s", ref, got.ID)
		}
		if got.Usage == nil {
			t.Error("single-user response should include usage")
		}
	}
}

func TestCreateUserConflictIs409(t *testing.T) {
	s, _ := testServer(t)
	do(t, s, "POST", "/api/users", `{"name":"alice"}`)

	rec := do(t, s, "POST", "/api/users", `{"name":"alice"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate name status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateUserBadInputIs400(t *testing.T) {
	s, _ := testServer(t)
	tests := map[string]string{
		"blank name":     `{"name":"  "}`,
		"bad uuid":       `{"name":"x","uuid":"nope"}`,
		"negative quota": `{"name":"y","quota_bytes":-5}`,
		"unknown field":  `{"name":"z","quota":100}`,
		"not json":       `{`,
		"empty body":     ``,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			rec := do(t, s, "POST", "/api/users", body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGetMissingUserIs404(t *testing.T) {
	s, _ := testServer(t)
	rec := do(t, s, "GET", "/api/users/nobody", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// An absent expires_at means "leave it alone"; an explicit null means "clear it".
// Without that distinction an operator could never remove an expiry.
func TestPatchDistinguishesAbsentFromNullExpiry(t *testing.T) {
	s, _ := testServer(t)
	do(t, s, "POST", "/api/users", `{"name":"alice","expires_at":"2030-01-01T00:00:00Z"}`)

	// Patch something unrelated: the expiry must survive.
	rec := do(t, s, "PATCH", "/api/users/alice", `{"note":"phone"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch note = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody[struct{ Result UserResponse }](t, rec)
	if got.Result.ExpiresAt == nil {
		t.Fatal("an unrelated patch cleared expires_at")
	}
	if got.Result.Note != "phone" {
		t.Errorf("note = %q", got.Result.Note)
	}

	// Explicit null clears it.
	rec = do(t, s, "PATCH", "/api/users/alice", `{"expires_at":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch null = %d: %s", rec.Code, rec.Body.String())
	}
	got = decodeBody[struct{ Result UserResponse }](t, rec)
	if got.Result.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil", got.Result.ExpiresAt)
	}
}

func TestPatchRename(t *testing.T) {
	s, _ := testServer(t)
	rec := do(t, s, "POST", "/api/users", `{"name":"alice"}`)
	created := decodeBody[struct{ Result UserResponse }](t, rec)

	rec = do(t, s, "PATCH", "/api/users/alice", `{"name":"alice2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody[struct{ Result UserResponse }](t, rec)
	if got.Result.Name != "alice2" {
		t.Errorf("name = %q", got.Result.Name)
	}
	// The id is stable, which is what keeps usage history attached across a rename.
	if got.Result.ID != created.Result.ID {
		t.Errorf("id changed on rename: %s -> %s", created.Result.ID, got.Result.ID)
	}
}

func TestDeleteUser(t *testing.T) {
	s, _ := testServer(t)
	do(t, s, "POST", "/api/users", `{"name":"alice"}`)

	rec := do(t, s, "DELETE", "/api/users/alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, "GET", "/api/users/alice", ""); rec.Code != http.StatusNotFound {
		t.Errorf("user still present after delete: %d", rec.Code)
	}
}

func TestListUsersIncludeUsage(t *testing.T) {
	s, st := testServer(t)
	rec := do(t, s, "POST", "/api/users", `{"name":"alice","quota_bytes":1000}`)
	created := decodeBody[struct{ Result UserResponse }](t, rec)

	if err := st.Usage.Add(t.Context(), time.Now(), []store.Delta{
		{UserID: created.Result.ID, Up: 100, Down: 250},
	}); err != nil {
		t.Fatal(err)
	}

	// Without include=usage the field is omitted entirely.
	rec = do(t, s, "GET", "/api/users", "")
	plain := decodeBody[struct{ Users []UserResponse }](t, rec)
	if len(plain.Users) != 1 {
		t.Fatalf("users = %d", len(plain.Users))
	}
	if plain.Users[0].Usage != nil {
		t.Error("usage should be omitted without include=usage")
	}

	rec = do(t, s, "GET", "/api/users?include=usage", "")
	withUsage := decodeBody[struct{ Users []UserResponse }](t, rec)
	u := withUsage.Users[0]
	if u.Usage == nil {
		t.Fatal("usage missing with include=usage")
	}
	if u.Usage.Total != 350 {
		t.Errorf("total = %d, want 350", u.Usage.Total)
	}
	if u.Usage.QuotaRemaining != 650 {
		t.Errorf("quota_remaining = %d, want 650", u.Usage.QuotaRemaining)
	}
}

func TestUserLink(t *testing.T) {
	s, _ := testServer(t)
	do(t, s, "POST", "/api/users", `{"name":"alice"}`)

	rec := do(t, s, "GET", "/api/users/alice/link", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("link = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody[struct{ Link string }](t, rec)
	for _, want := range []string{"vless://", "vpn.example.test:8443", "security=reality", "#alice"} {
		if !strings.Contains(got.Link, want) {
			t.Errorf("link %q missing %q", got.Link, want)
		}
	}
	// The private key must never appear in anything handed to a client.
	if strings.Contains(got.Link, testPrivateKey) {
		t.Error("link contains the Reality private key")
	}
}

func TestUserLinkIncludesQRMatrix(t *testing.T) {
	s, _ := testServer(t)
	do(t, s, "POST", "/api/users", `{"name":"alice"}`)

	rec := do(t, s, "GET", "/api/users/alice/link", "")
	got := decodeBody[LinkResponse](t, rec)
	if got.QR == nil {
		t.Fatal("qr matrix missing; a caller asking for a link usually needs to show it")
	}
	if got.QR.Size <= 0 || len(got.QR.Rows) != got.QR.Size {
		t.Fatalf("qr is not square: size=%d rows=%d", got.QR.Size, len(got.QR.Rows))
	}
	for i, row := range got.QR.Rows {
		if len(row) != got.QR.Size {
			t.Fatalf("qr row %d has width %d, want %d", i, len(row), got.QR.Size)
		}
	}
	if got.QR.QuietZone <= 0 {
		t.Error("quiet_zone should tell the caller what margin to add")
	}

	// Opt-out for callers that only want the URI.
	rec = do(t, s, "GET", "/api/users/alice/link?qr=false", "")
	plain := decodeBody[LinkResponse](t, rec)
	if plain.QR != nil {
		t.Error("qr=false should omit the matrix")
	}
	if plain.Link != got.Link {
		t.Error("the link itself should not depend on the qr parameter")
	}
}

// GET /api/server is the endpoint most likely to leak the private key by accident,
// since it is about exactly that key material.
func TestServerInfoNeverReturnsPrivateKey(t *testing.T) {
	s, _ := testServer(t)
	rec := do(t, s, "GET", "/api/server", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, testPrivateKey) {
		t.Fatal("GET /api/server returned the Reality private key")
	}
	if strings.Contains(body, "private") {
		t.Errorf("response mentions a private key field: %s", body)
	}

	got := decodeBody[ServerResponse](t, rec)
	// The derived public key, from the RFC 7748 vector this private key comes from.
	if got.PublicKey != testPublicKey {
		t.Errorf("public_key = %q", got.PublicKey)
	}
	if got.Host != "vpn.example.test" || got.Port != 8443 {
		t.Errorf("host/port = %s:%d", got.Host, got.Port)
	}
}

func TestTokenCreateAndAuthenticate(t *testing.T) {
	s, _ := testServer(t)

	rec := do(t, s, "POST", "/api/tokens", `{"label":"web"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token = %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeBody[struct {
		Token  store.Token `json:"token"`
		Secret string      `json:"secret"`
	}](t, rec)
	if created.Secret == "" {
		t.Fatal("no secret returned")
	}

	// The new token must work on the authenticated listener.
	req := httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer "+created.Secret)
	authRec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(authRec, req)
	if authRec.Code != http.StatusOK {
		t.Errorf("freshly created token rejected: %d", authRec.Code)
	}

	// Listing must never expose a secret, only its hash.
	rec = do(t, s, "GET", "/api/tokens", "")
	if strings.Contains(rec.Body.String(), created.Secret) {
		t.Error("GET /api/tokens returned the secret")
	}

	// Once deleted it stops working.
	if rec := do(t, s, "DELETE", "/api/tokens/"+created.Token.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete token = %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer "+created.Secret)
	authRec = httptest.NewRecorder()
	s.Handler(true).ServeHTTP(authRec, req)
	if authRec.Code != http.StatusNotFound {
		t.Errorf("deleted token still authenticates: %d", authRec.Code)
	}
}

func TestUsageEndpointValidatesInput(t *testing.T) {
	s, _ := testServer(t)
	do(t, s, "POST", "/api/users", `{"name":"alice"}`)

	tests := map[string]int{
		"/api/users/alice/usage":                               http.StatusOK,
		"/api/users/alice/usage?bucket=hour":                   http.StatusOK,
		"/api/users/alice/usage?bucket=day":                    http.StatusOK,
		"/api/users/alice/usage?bucket=week":                   http.StatusBadRequest,
		"/api/users/alice/usage?from=nonsense":                 http.StatusBadRequest,
		"/api/users/alice/usage?from=2026-01-02&to=2026-01-01": http.StatusBadRequest,
		"/api/users/alice/usage?from=2026-01-01&to=2026-01-02": http.StatusOK,
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			if rec := do(t, s, "GET", path, ""); rec.Code != want {
				t.Errorf("status = %d, want %d", rec.Code, want)
			}
		})
	}
}

func TestResetUsageEndpoint(t *testing.T) {
	s, _ := testServer(t)
	do(t, s, "POST", "/api/users", `{"name":"alice"}`)

	rec := do(t, s, "POST", "/api/users/alice/reset-usage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reset-usage = %d: %s", rec.Code, rec.Body.String())
	}
}

// ServeMux would answer 405 for a known path with a wrong verb, which confirms the path
// exists. The catch-all takes those too, so a wrong method is as uninformative as a wrong
// path.
func TestMethodMismatchIs404(t *testing.T) {
	s, _ := testServer(t)
	rec := do(t, s, "POST", "/api/server", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
