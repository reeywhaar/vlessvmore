package cli

import (
	"net/url"
	"testing"
)

func TestWithQuery(t *testing.T) {
	const base = "https://vpn.example.test/show/QK7M2X"

	tests := []struct {
		name   string
		raw    string
		params map[string]string
		want   string
	}{
		{
			// No flags: the URL has to come back byte for byte, so
			// `vlessvmore user install alice` keeps printing what it always did.
			name:   "nothing to add",
			raw:    base,
			params: map[string]string{"lang": "", "device": ""},
			want:   base,
		},
		{
			name:   "one parameter",
			raw:    base,
			params: map[string]string{"lang": "ru", "device": ""},
			want:   base + "?lang=ru",
		},
		{
			// Encoded, so the order is stable rather than following map iteration.
			name:   "both parameters",
			raw:    base,
			params: map[string]string{"lang": "ru", "device": "android"},
			want:   base + "?device=android&lang=ru",
		},
		{
			name:   "existing query survives",
			raw:    base + "?ref=chat",
			params: map[string]string{"lang": "ru"},
			want:   base + "?lang=ru&ref=chat",
		},
		{
			name:   "value needing escaping",
			raw:    base,
			params: map[string]string{"lang": "pt br"},
			want:   base + "?lang=pt+br",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := withQuery(tt.raw, tt.params)
			if err != nil {
				t.Fatalf("withQuery: %v", err)
			}
			if got != tt.want {
				t.Errorf("withQuery(%q) = %q, want %q", tt.raw, got, tt.want)
			}
			if _, err := url.Parse(got); err != nil {
				t.Errorf("result does not parse: %v", err)
			}
		})
	}
}
