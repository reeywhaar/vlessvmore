package api

import (
	"reflect"
	"testing"
)

// A half-translated page is worse than an obviously missing language, so every field of
// every shipped locale has to be filled in.
func TestLocalesAreComplete(t *testing.T) {
	for _, l := range locales {
		v := reflect.ValueOf(l)
		typ := v.Type()
		for i := range typ.NumField() {
			f := typ.Field(i)
			if f.Type.Kind() == reflect.Map {
				continue
			}
			if v.Field(i).String() == "" {
				t.Errorf("locale %s: %s is empty", l.Code, f.Name)
			}
		}
	}
}

func TestLocalesCoverEveryPlatform(t *testing.T) {
	for _, l := range locales {
		for _, p := range platforms {
			ps := l.Platform(p.ID)
			if ps.Label == "" || ps.Install == "" {
				t.Errorf("locale %s has no words for platform %q", l.Code, p.ID)
			}
		}
		if len(l.Platforms) != len(platforms) {
			t.Errorf("locale %s describes %d platforms, want %d",
				l.Code, len(l.Platforms), len(platforms))
		}
	}
}

func TestLocalesCoverEveryScreenshot(t *testing.T) {
	for _, l := range locales {
		for _, shot := range hiddifyShots {
			if l.Shot(shot) == "" {
				t.Errorf("locale %s has no alt text for %s", l.Code, shot)
			}
		}
		if len(l.Shots) != len(hiddifyShots) {
			t.Errorf("locale %s describes %d screenshots, want %d",
				l.Code, len(l.Shots), len(hiddifyShots))
		}
	}
}

// The fallback has to come first: it is what the switch offers before anything is known
// about the reader.
func TestFallbackLocaleIsFirst(t *testing.T) {
	if locales[0].Code != fallbackLocale {
		t.Errorf("first locale is %q, want %q", locales[0].Code, fallbackLocale)
	}
}

func TestPickLocale(t *testing.T) {
	tests := map[string]string{
		"":                        "en",
		"ru":                      "ru",
		"RU":                      "ru",
		"ru-RU":                   "ru",
		"ru-RU,ru;q=0.9,en;q=0.8": "ru",
		"en-US,ru;q=0.9":          "en",
		"de,ru;q=0.9":             "ru",
		"de":                      "en",
		"*":                       "en",
		"ru;q=0":                  "en",
		"ru;q=bogus":              "en",
		"  ru  ":                  "ru",
	}
	for header, want := range tests {
		t.Run(header, func(t *testing.T) {
			if got := pickLocale(header); got != want {
				t.Errorf("pickLocale(%q) = %q, want %q", header, got, want)
			}
		})
	}
}

func TestPickPlatform(t *testing.T) {
	tests := map[string]string{
		"":                                       platforms[0].ID,
		"Mozilla/5.0 (iPhone)":                   "ios",
		"Mozilla/5.0 (iPad)":                     "ios",
		"Mozilla/5.0 (Linux; Android 14; Pixel)": "android",
		"curl/8.4.0":                             platforms[0].ID,
		// Chrome on Android names both, and Android is the one that matters.
		"Mozilla/5.0 (Linux; Android 10) AppleWebKit/537.36 (KHTML, like Gecko) Chrome": "android",
	}
	for ua, want := range tests {
		t.Run(ua, func(t *testing.T) {
			if got := pickPlatform(ua); got != want {
				t.Errorf("pickPlatform(%q) = %q, want %q", ua, got, want)
			}
		})
	}
}

func TestStatusWordsExistForEveryState(t *testing.T) {
	for _, l := range locales {
		for _, state := range []string{"active", "disabled", "expired", "over_quota"} {
			if l.Status(state) == "" {
				t.Errorf("locale %s has no word for state %q", l.Code, state)
			}
		}
	}
}
