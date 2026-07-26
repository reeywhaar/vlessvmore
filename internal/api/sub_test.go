package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"vlessvmore/internal/store"
)

// getSub fetches a subscription URL through the *authenticated* handler, to prove the
// endpoint is exempt from bearer auth rather than merely reachable over the socket.
func getSub(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(rec, req)
	return rec
}

func newUserWithSub(t *testing.T, s *Server, body string) UserResponse {
	t.Helper()
	rec := do(t, s, "POST", "/api/users", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user = %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeBody[struct{ Result UserResponse }](t, rec)
	if got.Result.SubToken == "" {
		t.Fatal("created user has no subscription token")
	}
	return got.Result
}

// Subscription clients cannot send an Authorization header, so the token in the path has
// to be the whole credential.
func TestSubscriptionNeedsNoBearerToken(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	rec := getSub(t, s, SubPath+u.SubToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscriptionDefaultsToBase64(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	rec := getSub(t, s, SubPath+u.SubToken)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rec.Body.String()))
	if err != nil {
		t.Fatalf("body is not base64: %v\n%s", err, rec.Body.String())
	}
	uri := string(decoded)
	if !strings.HasPrefix(uri, "vless://") {
		t.Errorf("decoded body is not a vless URI: %q", uri)
	}
	if !strings.Contains(uri, "#alice") {
		t.Errorf("URI missing the display name: %q", uri)
	}
	if !strings.Contains(uri, "security=reality") {
		t.Errorf("URI missing reality params: %q", uri)
	}
}

func TestSubscriptionURIFormat(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	for _, format := range []string{"uri", "plain"} {
		rec := getSub(t, s, SubPath+u.SubToken+"?format="+format)
		if rec.Code != http.StatusOK {
			t.Fatalf("format=%s status = %d", format, rec.Code)
		}
		body := strings.TrimSpace(rec.Body.String())
		if !strings.HasPrefix(body, "vless://") {
			t.Errorf("format=%s body = %q, want a raw URI", format, body)
		}
	}

	if rec := getSub(t, s, SubPath+u.SubToken+"?format=clash"); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown format status = %d, want 400", rec.Code)
	}
}

// The headers are the reason to prefer a subscription over a pasted link: they are what
// makes a client display remaining traffic and an expiry date.
func TestSubscriptionUserinfoHeader(t *testing.T) {
	s, st := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice","quota_bytes":1000,"expires_at":"2030-06-01T00:00:00Z"}`)

	if err := st.Usage.Add(t.Context(), time.Now(), []store.Delta{
		{UserID: u.ID, Up: 120, Down: 480},
	}); err != nil {
		t.Fatal(err)
	}

	rec := getSub(t, s, SubPath+u.SubToken)
	info := rec.Header().Get("Subscription-Userinfo")
	if info == "" {
		t.Fatal("no Subscription-Userinfo header")
	}

	fields := map[string]int64{}
	for _, part := range strings.Split(info, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			t.Fatalf("malformed field %q in %q", part, info)
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("field %s is not a number in %q", k, info)
		}
		fields[k] = n
	}

	if fields["upload"] != 120 {
		t.Errorf("upload = %d, want 120", fields["upload"])
	}
	if fields["download"] != 480 {
		t.Errorf("download = %d, want 480", fields["download"])
	}
	if fields["total"] != 1000 {
		t.Errorf("total = %d, want the quota 1000", fields["total"])
	}
	want := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC).Unix()
	if fields["expire"] != want {
		t.Errorf("expire = %d, want %d", fields["expire"], want)
	}

	if got := rec.Header().Get("Profile-Update-Interval"); got != "24" {
		t.Errorf("Profile-Update-Interval = %q, want 24", got)
	}
	title := rec.Header().Get("Profile-Title")
	if !strings.HasPrefix(title, "base64:") {
		t.Fatalf("Profile-Title = %q, want a base64: prefix", title)
	}
	name, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(title, "base64:"))
	if err != nil || string(name) != "alice" {
		t.Errorf("Profile-Title decodes to %q (err %v), want alice", name, err)
	}

	// A cached credential would outlive its revocation.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// An unlimited user must produce *no* total field. Sending total=0 is the documented
// convention for "unlimited", but Hiddify responds to it by inventing a ceiling of its own
// — it displayed ~85.9 GiB remaining for a user with no quota at all. Omitting the field
// gives the client nothing to misread.
func TestSubscriptionUserinfoOmitsUnsetLimits(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	rec := getSub(t, s, SubPath+u.SubToken)
	info := rec.Header().Get("Subscription-Userinfo")

	if strings.Contains(info, "total") {
		t.Errorf("an unlimited user must not advertise a total, got %q", info)
	}
	if strings.Contains(info, "expire") {
		t.Errorf("a non-expiring user must not advertise an expiry, got %q", info)
	}
	// Traffic used is still worth reporting; it is the *limits* that are absent.
	if !strings.Contains(info, "upload=0") || !strings.Contains(info, "download=0") {
		t.Errorf("usage should still be reported, got %q", info)
	}
}

// A quota with no expiry advertises the quota only, and vice versa.
func TestSubscriptionUserinfoOmitsOnlyWhatIsUnset(t *testing.T) {
	s, _ := testServer(t)

	quotaOnly := newUserWithSub(t, s, `{"name":"quota-only","quota_bytes":1000}`)
	info := getSub(t, s, SubPath+quotaOnly.SubToken).Header().Get("Subscription-Userinfo")
	if !strings.Contains(info, "total=1000") {
		t.Errorf("quota should be advertised, got %q", info)
	}
	if strings.Contains(info, "expire") {
		t.Errorf("no expiry set, so none should be advertised, got %q", info)
	}

	expiryOnly := newUserWithSub(t, s, `{"name":"expiry-only","expires_at":"2030-06-01T00:00:00Z"}`)
	info = getSub(t, s, SubPath+expiryOnly.SubToken).Header().Get("Subscription-Userinfo")
	if strings.Contains(info, "total") {
		t.Errorf("no quota set, so none should be advertised, got %q", info)
	}
	want := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC).Unix()
	if !strings.Contains(info, fmt.Sprintf("expire=%d", want)) {
		t.Errorf("expiry should be advertised, got %q", info)
	}
}

// An unknown token must be indistinguishable from any other unmatched path, so poking
// at it reveals nothing about what this server is.
func TestSubscriptionUnknownTokenIs404(t *testing.T) {
	s, _ := testServer(t)
	newUserWithSub(t, s, `{"name":"alice"}`)

	for _, token := range []string{"nope", strings.Repeat("A", 32)} {
		rec := getSub(t, s, SubPath+token)
		if rec.Code != http.StatusNotFound {
			t.Errorf("token %q = %d, want 404", token, rec.Code)
		}
		if strings.Contains(strings.ToLower(rec.Body.String()), "vless") {
			t.Errorf("404 body leaks what this service is: %s", rec.Body.String())
		}
	}

	// A traversal attempt is normalised by ServeMux into a redirect to the cleaned
	// path, which is fine — what matters is that it never serves a credential.
	rec := getSub(t, s, SubPath+"../etc/passwd")
	if rec.Code == http.StatusOK {
		t.Errorf("path traversal returned 200: %s", rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "vless") {
		t.Errorf("traversal response leaks a credential: %s", rec.Body.String())
	}
}

// A disabled user still gets their link plus honest headers: the credential is already
// theirs and will not work, and the headers are what let a client explain why.
func TestSubscriptionServesDisabledUserWithHonestHeaders(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice","quota_bytes":100}`)

	if rec := do(t, s, "PATCH", "/api/users/alice", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
	}

	rec := getSub(t, s, SubPath+u.SubToken)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 so the client can show why it is not working", rec.Code)
	}
	if rec.Header().Get("Subscription-Userinfo") == "" {
		t.Error("no Subscription-Userinfo header for a disabled user")
	}
}

func TestRotateSubInvalidatesOldURL(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)
	old := u.SubToken

	rec := do(t, s, "POST", "/api/users/alice/rotate-sub", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", rec.Code, rec.Body.String())
	}
	rotated := decodeBody[UserResponse](t, rec)
	if rotated.SubToken == old {
		t.Fatal("token did not change")
	}
	if rotated.UUID != u.UUID {
		t.Error("rotating the subscription token changed the credential")
	}

	if rec := getSub(t, s, SubPath+old); rec.Code != http.StatusNotFound {
		t.Errorf("the old URL still works: %d", rec.Code)
	}
	if rec := getSub(t, s, SubPath+rotated.SubToken); rec.Code != http.StatusOK {
		t.Errorf("the new URL does not work: %d", rec.Code)
	}
}

func TestSubscriptionURLExposedOnUserResponses(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	// Defaults to https://<host> when subscription_url_base is unset, which is right
	// for the documented topology where a proxy fronts this on the Reality hostname.
	want := "https://vpn.example.test" + SubPath + u.SubToken
	if u.SubscriptionURL != want {
		t.Errorf("create response subscription_url = %q, want %q", u.SubscriptionURL, want)
	}

	rec := do(t, s, "GET", "/api/users/alice", "")
	got := decodeBody[UserResponse](t, rec)
	if got.SubscriptionURL != want {
		t.Errorf("get response subscription_url = %q, want %q", got.SubscriptionURL, want)
	}

	rec = do(t, s, "GET", "/api/users/alice/link", "")
	link := decodeBody[LinkResponse](t, rec)
	if link.SubscriptionURL != want {
		t.Errorf("link response subscription_url = %q, want %q", link.SubscriptionURL, want)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := map[string]string{
		"alice":       "alice",
		"alice smith": "alice-smith",
		// Dots are replaced too, so ".." can never reach a client as a filename.
		"alice/../../etc": "alice-------etc",
		// strings.Map works on runes, so four Cyrillic letters become four dashes.
		"файл":            "----",
		"":                "subscription",
		"ok_name-1.2":     "ok_name-1-2",
		`quote"and\slash`: "quote-and-slash",
	}
	for in, want := range tests {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// A user seeing their own name as the profile label is odd — they know who they are.
// A configured server name replaces it in both places a client reads: the vless://
// fragment and the subscription's Profile-Title.
func TestServerNameOverridesUserNameInClientLabels(t *testing.T) {
	s, _ := testServer(t)
	s.cfg.Name = "Reey VPN"

	u := newUserWithSub(t, s, `{"name":"alice"}`)

	rec := getSub(t, s, SubPath+u.SubToken)
	title, err := base64.StdEncoding.DecodeString(
		strings.TrimPrefix(rec.Header().Get("Profile-Title"), "base64:"))
	if err != nil {
		t.Fatalf("Profile-Title is not base64: %v", err)
	}
	if string(title) != "Reey VPN" {
		t.Errorf("Profile-Title = %q, want the server name", title)
	}

	// The URI fragment must agree, so the label is the same however the profile was
	// added — scanned, pasted, or subscribed.
	uri := getSub(t, s, SubPath+u.SubToken+"?format=uri").Body.String()
	if !strings.Contains(uri, "#Reey%20VPN") {
		t.Errorf("URI fragment should be the server name, got: %s", strings.TrimSpace(uri))
	}
	if strings.Contains(uri, "#alice") {
		t.Errorf("URI still labelled with the user's name: %s", strings.TrimSpace(uri))
	}

	// The suggested filename follows the same label, sanitised.
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "Reey-VPN.txt") {
		t.Errorf("Content-Disposition = %q, want the server name", cd)
	}
}

// Without a server name, nothing changes: the user's own name is still the label.
func TestClientLabelFallsBackToUserName(t *testing.T) {
	s, _ := testServer(t)
	if s.cfg.Name != "" {
		t.Fatalf("test config should have no server name, got %q", s.cfg.Name)
	}

	u := newUserWithSub(t, s, `{"name":"alice"}`)
	rec := getSub(t, s, SubPath+u.SubToken)
	title, err := base64.StdEncoding.DecodeString(
		strings.TrimPrefix(rec.Header().Get("Profile-Title"), "base64:"))
	if err != nil {
		t.Fatal(err)
	}
	if string(title) != "alice" {
		t.Errorf("Profile-Title = %q, want the user's name as a fallback", title)
	}

	uri := getSub(t, s, SubPath+u.SubToken+"?format=uri").Body.String()
	if !strings.Contains(uri, "#alice") {
		t.Errorf("URI fragment should fall back to the user's name, got: %s", strings.TrimSpace(uri))
	}
}
