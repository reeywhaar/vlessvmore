package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// getPage fetches through the authenticated handler, to prove the route is exempt rather
// than merely reachable over the socket.
func getPage(t *testing.T, s *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler(true).ServeHTTP(rec, req)
	return rec
}

func TestShowPageNeedsNoBearerToken(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	rec := getPage(t, s, ShowPath+u.SubToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	// The page embeds a credential; an intermediary must not keep a copy.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestShowUnknownTokenIsTheSame404(t *testing.T) {
	s, _ := testServer(t)
	newUserWithSub(t, s, `{"name":"alice"}`)

	want := getPage(t, s, "/nonsense", nil)
	got := getPage(t, s, ShowPath+"NOSUCHTOKEN", nil)

	if got.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", got.Code)
	}
	if got.Body.String() != want.Body.String() {
		t.Errorf("body = %q, want the unmatched-path body %q", got.Body.String(), want.Body.String())
	}
}

// Every language and every device is in the document, hidden. A reader with JavaScript
// off must still land on exactly one set of instructions, not all of them stacked.
func TestShowPageRevealsOnePaneOfEachKind(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	body := getPage(t, s, ShowPath+u.SubToken, nil).Body.String()

	sections := strings.Split(body, `<main class="lang"`)[1:]
	if len(sections) != len(locales) {
		t.Fatalf("found %d language sections, want %d", len(sections), len(locales))
	}

	visibleLangs := 0
	for _, section := range sections {
		if !strings.Contains(firstTag(section), " hidden") {
			visibleLangs++
		}
		// Each language carries its own device switch, so the count is per-section.
		// JavaScript keeps them in step; the server only has to get one right.
		if got := countVisible(section, "data-device"); got != 1 {
			t.Errorf("%d device panes visible in one language, want exactly 1", got)
		}
	}
	if visibleLangs != 1 {
		t.Errorf("%d languages visible, want exactly 1", visibleLangs)
	}
}

// firstTag returns the rest of the opening tag the section was split on.
func firstTag(section string) string {
	tag, _, _ := strings.Cut(section, ">")
	return tag
}

func countVisible(html, attr string) int {
	tags := regexp.MustCompile(`<[a-z]+[^>]*\s`+attr+`="[a-z-]+"[^>]*>`).FindAllString(html, -1)
	n := 0
	for _, tag := range tags {
		if !strings.Contains(tag, " hidden") {
			n++
		}
	}
	return n
}

func TestShowPageDefaultsFromRequestHeaders(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	tests := []struct {
		name       string
		headers    map[string]string
		wantLang   string
		wantDevice string
	}{
		{"no hints", nil, "en", platforms[0].ID},
		{"russian", map[string]string{"Accept-Language": "ru-RU,ru;q=0.9"}, "ru", platforms[0].ID},
		{"unknown language", map[string]string{"Accept-Language": "de"}, "en", platforms[0].ID},
		{"android", map[string]string{"User-Agent": "Mozilla/5.0 (Linux; Android 14)"}, "en", "android"},
		{"iphone", map[string]string{"User-Agent": "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0)"}, "en", "ios"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := getPage(t, s, ShowPath+u.SubToken, tt.headers).Body.String()

			if want := `data-lang="` + tt.wantLang + `">`; !strings.Contains(body, want) {
				t.Errorf("no visible pane for language %q", tt.wantLang)
			}
			if want := `data-device="` + tt.wantDevice + `">`; !strings.Contains(body, want) {
				t.Errorf("no visible pane for device %q", tt.wantDevice)
			}
		})
	}
}

// ?lang= and ?device= are for sending a link to someone whose language and phone you
// already know, so they have to win over whatever the browser says about itself.
func TestShowPageQueryOverridesHeaders(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	// Headers that would otherwise produce English on an iPhone.
	headers := map[string]string{
		"Accept-Language": "en-US,en;q=0.9",
		"User-Agent":      "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0)",
	}

	tests := []struct {
		name       string
		query      string
		wantLang   string
		wantDevice string
	}{
		{"no query", "", "en", "ios"},
		{"language only", "?lang=ru", "ru", "ios"},
		{"device only", "?device=android", "en", "android"},
		{"both", "?lang=ru&device=android", "ru", "android"},
		{"uppercase", "?lang=RU&device=ANDROID", "ru", "android"},
		// A typo should still give a usable page, not an error page.
		{"unknown language", "?lang=de", "en", "ios"},
		{"unknown device", "?device=blackberry", "en", "ios"},
		{"empty values", "?lang=&device=", "en", "ios"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := getPage(t, s, ShowPath+u.SubToken+tt.query, headers).Body.String()

			if want := `data-lang="` + tt.wantLang + `">`; !strings.Contains(body, want) {
				t.Errorf("no visible pane for language %q", tt.wantLang)
			}
			if want := `data-device="` + tt.wantDevice + `">`; !strings.Contains(body, want) {
				t.Errorf("no visible pane for device %q", tt.wantDevice)
			}
		})
	}
}

// The script restores a choice from localStorage, which would otherwise silently undo a
// link someone was deliberately sent. These attributes are how it knows not to.
func TestShowPageMarksForcedChoices(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	forced := getPage(t, s, ShowPath+u.SubToken+"?lang=ru&device=android", nil).Body.String()
	if !strings.Contains(forced, `data-forced-lang="ru"`) {
		t.Error("a forced language is not marked for the script")
	}
	if !strings.Contains(forced, `data-forced-device="android"`) {
		t.Error("a forced device is not marked for the script")
	}

	// Detected, not forced: a remembered choice should still win.
	detected := getPage(t, s, ShowPath+u.SubToken, nil).Body.String()
	if strings.Contains(detected, "data-forced-") {
		t.Error("a detected choice was marked as forced")
	}

	// A value we do not ship must not be echoed back into the document.
	bogus := getPage(t, s, ShowPath+u.SubToken+"?lang=de&device=blackberry", nil).Body.String()
	if strings.Contains(bogus, "data-forced-") {
		t.Error("an unknown value was marked as forced")
	}
}

func TestShowPageCarriesTheCredential(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	body := getPage(t, s, ShowPath+u.SubToken, nil).Body.String()

	if !strings.Contains(body, u.SubscriptionURL) {
		t.Error("the page does not show the subscription URL to copy")
	}
	if !strings.Contains(body, "hiddify://import?") {
		t.Error("the page has no one-tap import link")
	}
	if !strings.Contains(body, url.QueryEscape(u.SubscriptionURL)) {
		t.Error("the import link does not carry the subscription URL")
	}
	if !strings.Contains(body, "<svg") {
		t.Error("the page has no QR code")
	}
	// Assets are referenced with the token, or they will 404 when the browser asks.
	if !strings.Contains(body, StaticPath+"app.") || !strings.Contains(body, "token="+u.SubToken) {
		t.Error("assets are not referenced with a token")
	}
}

// html/template drops a URL whose scheme it does not recognise unless it is typed as
// template.URL, and silently — the button would just stop working.
func TestDeepLinkSurvivesEscaping(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	body := getPage(t, s, ShowPath+u.SubToken, nil).Body.String()
	if strings.Contains(body, "ZgotmplZ") {
		t.Error("the deep link was neutered by html/template")
	}
}

func TestShowPageEscapesTheUserName(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"<script>alert(1)</script>"}`)

	body := getPage(t, s, ShowPath+u.SubToken, nil).Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a user name was rendered as markup")
	}
}

func TestHiddifyImportLink(t *testing.T) {
	got := hiddifyImportLink("https://vpn.example.test/sub/ABC", "Home")

	// Hiddify's parser needs an authority before it looks at anything, and reads the
	// url query parameter in preference to the path.
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if parsed.Scheme != "hiddify" {
		t.Errorf("scheme = %q, want hiddify", parsed.Scheme)
	}
	if parsed.Host == "" {
		t.Errorf("%q has no authority; Hiddify will ignore it", got)
	}
	if u := parsed.Query().Get("url"); u != "https://vpn.example.test/sub/ABC" {
		t.Errorf("url param = %q", u)
	}
	if n := parsed.Query().Get("name"); n != "Home" {
		t.Errorf("name param = %q", n)
	}
}

func TestQRSVGCoversEveryDarkModule(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	body := getPage(t, s, ShowPath+u.SubToken, nil).Body.String()

	svg := body[strings.Index(body, "<svg"):]
	svg = svg[:strings.Index(svg, "</svg>")]

	// Runs are merged, so the count is not the module count — but a matrix this size
	// cannot come out as a handful of rectangles unless the drawing is broken.
	if n := strings.Count(svg, "<rect"); n < 50 {
		t.Errorf("QR has %d rects, which is too few to be a real code", n)
	}
	if !strings.Contains(svg, "viewBox=") {
		t.Error("QR has no viewBox, so it will not scale")
	}
}
