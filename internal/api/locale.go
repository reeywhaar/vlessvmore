package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed web/locales/*.json
var localeFS embed.FS

// fallbackLocale is the code used when Accept-Language matches nothing, and the one
// listed first in the switch.
const fallbackLocale = "en"

// Strings is one language's copy of the install page.
//
// A struct rather than map[string]string so a missing key fails a test instead of
// rendering an empty element. Adding a language means adding web/locales/<code>.json and
// nothing else; adding a *string* means a field here and a line in every locale file,
// which is the point — a half-translated page is worse than an obviously missing one.
type Strings struct {
	Code string `json:"code"`
	Name string `json:"name"` // Native name, shown on the language switch.

	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`

	StepInstall     string `json:"step_install"`
	StepInstallBody string `json:"step_install_body"`
	StoreButton     string `json:"store_button"`
	DirectButton    string `json:"direct_button"`

	StepAdd     string `json:"step_add"`
	StepAddBody string `json:"step_add_body"`
	AddButton   string `json:"add_button"`

	ManualTitle string `json:"manual_title"`
	ManualBody  string `json:"manual_body"`
	CopyButton  string `json:"copy_button"`
	CopiedLabel string `json:"copied_label"`

	QRTitle string `json:"qr_title"`
	QRBody  string `json:"qr_body"`

	StepConnect     string `json:"step_connect"`
	StepConnectBody string `json:"step_connect_body"`

	StepDone     string `json:"step_done"`
	StepDoneBody string `json:"step_done_body"`

	AccountTitle    string `json:"account_title"`
	AccountName     string `json:"account_name"`
	AccountUsed     string `json:"account_used"`
	AccountQuota    string `json:"account_quota"`
	AccountExpires  string `json:"account_expires"`
	AccountStatus   string `json:"account_status"`
	StatusActive    string `json:"status_active"`
	StatusDisabled  string `json:"status_disabled"`
	StatusExpired   string `json:"status_expired"`
	StatusOverQuota string `json:"status_over_quota"`
	Unlimited       string `json:"unlimited"`
	Never           string `json:"never"`

	LanguageLabel string `json:"language_label"`
	DeviceLabel   string `json:"device_label"`

	// Platforms is keyed by Platform.ID, Shots by screenshot name. Both are checked
	// against clients.go by TestLocalesCoverEveryPlatform.
	Platforms map[string]PlatformStrings `json:"platforms"`
	Shots     map[string]string          `json:"shots"`
}

// PlatformStrings is the part of a translation that differs per OS.
type PlatformStrings struct {
	Label   string `json:"label"`
	Install string `json:"install"`
}

// Platform looks up one device's words. Template helper; a missing id renders empty
// rather than failing the page, and TestLocalesCoverEveryPlatform makes sure that never
// happens in the first place.
func (s Strings) Platform(id string) PlatformStrings { return s.Platforms[id] }

// Shot is a screenshot's alt text.
func (s Strings) Shot(name string) string { return s.Shots[name] }

// Status renders one of the account states from accountState.
func (s Strings) Status(state string) string {
	switch state {
	case "disabled":
		return s.StatusDisabled
	case "expired":
		return s.StatusExpired
	case "over_quota":
		return s.StatusOverQuota
	}
	return s.StatusActive
}

// localeByCode returns a shipped locale, falling back rather than failing.
func localeByCode(code string) Strings {
	for _, l := range locales {
		if l.Code == code {
			return l
		}
	}
	return locales[0]
}

// knownLocale echoes a language code this build ships, or "" for anything else.
func knownLocale(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	for _, l := range locales {
		if l.Code == code {
			return l.Code
		}
	}
	return ""
}

// locales is every shipped translation, fallback first and the rest by code. Built once
// at init: a malformed locale file is a programming error, not a runtime condition.
var locales = mustLoadLocales()

func mustLoadLocales() []Strings {
	out, err := loadLocales()
	if err != nil {
		panic(err)
	}
	return out
}

func loadLocales() ([]Strings, error) {
	entries, err := localeFS.ReadDir("web/locales")
	if err != nil {
		return nil, err
	}

	var out []Strings
	for _, e := range entries {
		raw, err := localeFS.ReadFile(path.Join("web/locales", e.Name()))
		if err != nil {
			return nil, err
		}
		var s Strings
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return nil, fmt.Errorf("locale %s: %w", e.Name(), err)
		}
		if want := strings.TrimSuffix(e.Name(), ".json"); s.Code != want {
			return nil, fmt.Errorf("locale %s declares code %q", e.Name(), s.Code)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no locales embedded")
	}

	sort.Slice(out, func(i, j int) bool {
		if (out[i].Code == fallbackLocale) != (out[j].Code == fallbackLocale) {
			return out[i].Code == fallbackLocale
		}
		return out[i].Code < out[j].Code
	})
	if out[0].Code != fallbackLocale {
		return nil, fmt.Errorf("fallback locale %q is missing", fallbackLocale)
	}
	return out, nil
}

// pickLocale chooses a language from an Accept-Language header.
//
// Primary subtags only, so "ru-RU" and "ru" are the same choice — this is a five-string
// page, not a place to care about regional variants. Unparseable q-values sort last
// rather than failing the request.
func pickLocale(header string) string {
	type candidate struct {
		tag string
		q   float64
	}
	var wanted []candidate

	for _, part := range strings.Split(header, ",") {
		tag, rest, _ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(rest), "q="); ok {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				q = 0
			} else {
				q = parsed
			}
		}
		if primary, _, found := strings.Cut(tag, "-"); found {
			tag = primary
		}
		wanted = append(wanted, candidate{tag: tag, q: q})
	}

	sort.SliceStable(wanted, func(i, j int) bool { return wanted[i].q > wanted[j].q })

	for _, c := range wanted {
		if c.q <= 0 {
			break
		}
		if c.tag == "*" {
			break
		}
		for _, l := range locales {
			if l.Code == c.tag {
				return l.Code
			}
		}
	}
	return fallbackLocale
}

// pickPlatform guesses the device from a user agent, falling back to the first entry in
// the switch. A wrong guess costs one tap.
func pickPlatform(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "android"):
		return "android"
	case strings.Contains(ua, "iphone"),
		strings.Contains(ua, "ipad"),
		strings.Contains(ua, "ipod"):
		return "ios"
	}
	return platforms[0].ID
}
