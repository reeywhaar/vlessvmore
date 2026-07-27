package api

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vlessvmore/internal/bytesize"
	"vlessvmore/internal/link"
	"vlessvmore/internal/store"
)

//go:embed web/page.html.tmpl
var pageFS embed.FS

var pageTemplate = template.Must(template.New("page.html.tmpl").ParseFS(pageFS, "web/page.html.tmpl"))

// ShowPath is the prefix for the install page. Like /sub/, the token in the path is the
// whole credential.
const ShowPath = "/show/"

// LangParam and DeviceParam override what the page would otherwise guess from the request
// headers.
const (
	LangParam   = "lang"
	DeviceParam = "device"
)

// pageData is everything page.html.tmpl renders.
//
// Every language and every device is rendered into one response, with all but the chosen pair
// marked hidden. The switch then works without a round trip, and a browser with no
// JavaScript still shows exactly one correct set of instructions.
type pageData struct {
	Lang      string
	Device    string
	Locales   []Strings
	Platforms []Platform

	// ForcedLang and ForcedDevice are set when the URL said which to show. They tell the
	// page's script not to let a choice remembered from an earlier visit quietly
	// override the link someone was deliberately sent.
	ForcedLang   string
	ForcedDevice string

	// Current is the chosen locale, for the parts of the document that exist once:
	// <title> and <html lang>. JavaScript updates the title when the switch is used.
	Current Strings

	DeepLink template.URL
	SubURL   string
	QR       template.HTML

	CSS   string
	JS    string
	Shots map[string]string // Screenshot name to its asset URL.
	ShotW int
	ShotH int

	Account account
}

// account is the status block, pre-rendered because the numbers need a locale's words
// around them.
type account struct {
	Name    string
	Used    string
	Quota   string
	Expires *time.Time
	State   string // active | disabled | expired | over_quota
}

// show serves the install page for one subscription token.
func (s *Server) show(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	u, err := s.store.Users.GetBySubToken(token)
	if err != nil {
		s.notFound(w, r)
		return
	}

	usage, err := s.usageSummary(r, u)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	subURL := s.SubscriptionURL(u)
	qr, err := link.Encode(subURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// ?lang= and ?device= win over the headers, for handing someone a link when you already
	// know what they read and what they carry. An unknown value falls back to detection
	// rather than erroring: the page is for people, and a typo should still be useful.
	forcedLang := knownLocale(r.URL.Query().Get(LangParam))
	forcedDevice := knownPlatform(r.URL.Query().Get(DeviceParam))

	lang := forcedLang
	if lang == "" {
		lang = pickLocale(r.Header.Get("Accept-Language"))
	}
	device := forcedDevice
	if device == "" {
		device = pickPlatform(r.Header.Get("User-Agent"))
	}

	shots := make(map[string]string, len(hiddifyShots))
	for _, name := range hiddifyShots {
		shots[name] = assetURL(name+".webp", token)
	}

	data := pageData{
		Lang:         lang,
		Device:       device,
		ForcedLang:   forcedLang,
		ForcedDevice: forcedDevice,
		Locales:      locales,
		Platforms:    platforms,
		Current:      localeByCode(lang),
		DeepLink:     template.URL(hiddifyImportLink(subURL, s.cfg.ClientLabel(u.Name))),
		SubURL:       subURL,
		QR:           qrSVG(qr),
		CSS:          assetURL("app.css", token),
		JS:           assetURL("app.js", token),
		Shots:        shots,
		ShotW:        shotWidth,
		ShotH:        shotHeight,
		Account: account{
			Name:    u.Name,
			Used:    bytesize.Format(usage.WindowTotal),
			Quota:   quotaLabel(u.QuotaBytes),
			Expires: u.ExpiresAt,
			State:   accountState(u, time.Now()),
		},
	}

	// The page embeds a credential, so it must not outlive the tab it was opened in.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := pageTemplate.Execute(w, data); err != nil {
		s.log.Error("rendering the install page", "error", err)
	}
}

// quotaLabel returns an empty string for "no limit", which the template turns into the
// locale's own word for it.
func quotaLabel(n int64) string {
	if n <= 0 {
		return ""
	}
	return bytesize.Format(n)
}

func accountState(u *store.User, now time.Time) string {
	switch {
	case u.ExpiresAt != nil && now.After(*u.ExpiresAt):
		return "expired"
	case u.DisabledReason == "quota":
		return "over_quota"
	case !u.Enabled:
		return "disabled"
	}
	return "active"
}

// hiddifyImportLink builds the one-tap import URL.
//
// Hiddify registers the scheme on both platforms and its parser (LinkParser.deep) needs
// an authority before it will look at anything, hence the "import" host, which it then
// ignores. It checks for a `url` query parameter before falling back to reading the path,
// and the query form is the safer of the two: percent-encoding leaves no question about
// how the "//" inside a nested URL survives the trip through the OS.
//
// The path form, if it is ever needed: "hiddify://import/" + subURL + "#" + name.
func hiddifyImportLink(subURL, name string) string {
	q := url.Values{}
	q.Set("url", subURL)
	q.Set("name", name)
	return "hiddify://import?" + q.Encode()
}

// qrSVG draws a QR matrix as an inline SVG.
//
// Inline so the page needs no image request for it, and one rect per run of dark modules
// rather than one per module — same picture, a fifth of the bytes.
func qrSVG(q *link.QR) template.HTML {
	side := q.Size + 2*q.QuietZone

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" `+
		`shape-rendering="crispEdges" role="img" aria-hidden="true">`, side, side)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><g fill="#000">`, side, side)

	for y, row := range q.Rows {
		x := 0
		for x < len(row) {
			if row[x] != '1' {
				x++
				continue
			}
			run := 0
			for x+run < len(row) && row[x+run] == '1' {
				run++
			}
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="1"/>`,
				x+q.QuietZone, y+q.QuietZone, run)
			x += run
		}
	}
	b.WriteString(`</g></svg>`)
	return template.HTML(b.String())
}

// ShowURL is the install page to hand to a user.
func (s *Server) ShowURL(u *store.User) string {
	return ShowURL(s.cfg.SubscriptionBase(), u)
}

// ShowURL builds a user's install page URL on the given origin.
func ShowURL(base string, u *store.User) string {
	if u.SubToken == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + ShowPath + u.SubToken
}
