package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A dump is a file people read and diff. `"tokens": null` beside an omitted `usage` is two
// spellings of the same "not exported" state, and `"users": null` for an empty deployment
// makes a consumer handle a case that should not exist.
func TestDumpEncodesNoNulls(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		opts ExportOptions
	}{
		{"defaults", ExportOptions{}},
		{"everything", ExportOptions{IncludeUsage: true, IncludeTokens: true}},
		{"no identity", ExportOptions{ExcludeIdentity: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Deliberately an empty store: nil slices are what produce nulls.
			s := open(t)
			dump, err := s.Export(ctx, now, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := dump.Encode(&buf); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(buf.String(), "null") {
				t.Errorf("dump contains a null:\n%s", buf.String())
			}

			// users is the one always-present section, and must be an array.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			if got := string(raw["users"]); got != "[]" {
				t.Errorf(`users = %s, want []`, got)
			}
		})
	}
}

// The distinction a plain slice cannot express, and the reason this is not merely
// cosmetic: exporting a deployment that happens to have zero tokens must still clear the
// destination's tokens on restore, or old API credentials survive a --force import.
func TestExportedEmptyTokensClearsDestination(t *testing.T) {
	ctx := context.Background()

	src := open(t)
	if _, err := src.Users.Create(CreateParams{Name: "alice"}, now); err != nil {
		t.Fatal(err)
	}
	// Source has users but no tokens.
	dump, err := src.Export(ctx, now, ExportOptions{IncludeTokens: true})
	if err != nil {
		t.Fatal(err)
	}
	if dump.Tokens == nil {
		t.Fatal("an explicit token export must be present even when empty")
	}
	if len(*dump.Tokens) != 0 {
		t.Fatalf("expected zero tokens, got %d", len(*dump.Tokens))
	}

	// It has to survive the JSON round trip as present-and-empty, not vanish.
	var buf bytes.Buffer
	if err := dump.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"tokens": []`) {
		t.Errorf("empty-but-exported tokens should encode as []:\n%s", buf.String())
	}
	read, err := ReadDump(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if read.Tokens == nil {
		t.Fatal("tokens became absent across the round trip; the destination's would survive")
	}

	dst := open(t)
	if _, _, err := dst.Tokens.Create("stale", now); err != nil {
		t.Fatal(err)
	}
	if err := dst.Import(ctx, read, true); err != nil {
		t.Fatal(err)
	}
	if n := len(dst.Tokens.List()); n != 0 {
		t.Errorf("%d stale token(s) survived a restore from a token-less source", n)
	}
}

// The other half of the tri-state: not exporting tokens leaves the destination's alone.
func TestUnexportedTokensAreLeftAlone(t *testing.T) {
	ctx := context.Background()

	src := open(t)
	dump, err := src.Export(ctx, now, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dump.Tokens != nil {
		t.Fatal("tokens should be absent by default")
	}

	dst := open(t)
	if _, _, err := dst.Tokens.Create("keep-me", now); err != nil {
		t.Fatal(err)
	}
	if err := dst.Import(ctx, dump, true); err != nil {
		t.Fatal(err)
	}
	if n := len(dst.Tokens.List()); n != 1 {
		t.Errorf("tokens = %d, want the destination's own kept", n)
	}
}
