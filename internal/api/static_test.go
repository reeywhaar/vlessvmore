package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestStaticNeedsAToken(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	hashed := assetNames["app.css"]
	if hashed == "" {
		t.Fatal("app.css was not embedded")
	}

	tests := map[string]int{
		StaticPath + hashed + "?token=" + u.SubToken: http.StatusOK,
		StaticPath + hashed:                          http.StatusNotFound,
		StaticPath + hashed + "?token=nope":          http.StatusNotFound,
		StaticPath + "app.css?token=" + u.SubToken:   http.StatusNotFound,
		StaticPath + "app.js":                        http.StatusNotFound,
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			if rec := getPage(t, s, path, nil); rec.Code != want {
				t.Errorf("status = %d, want %d", rec.Code, want)
			}
		})
	}
}

func TestStaticServesEveryEmbeddedType(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)

	tests := map[string]string{
		"app.css":      "text/css; charset=utf-8",
		"app.js":       "text/javascript; charset=utf-8",
		"connect.webp": "image/webp",
	}
	for name, wantType := range tests {
		t.Run(name, func(t *testing.T) {
			rec := getPage(t, s, StaticPath+assetNames[name]+"?token="+u.SubToken, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != wantType {
				t.Errorf("Content-Type = %q, want %q", ct, wantType)
			}
			if rec.Body.Len() == 0 {
				t.Error("empty body")
			}
			// The URL carries a credential, so a shared cache must not keep it.
			if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
				t.Errorf("Cache-Control = %q, want private", cc)
			}
		})
	}
}

func TestStaticRevalidates(t *testing.T) {
	s, _ := testServer(t)
	u := newUserWithSub(t, s, `{"name":"alice"}`)
	path := StaticPath + assetNames["app.css"] + "?token=" + u.SubToken

	first := getPage(t, s, path, nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	second := getPage(t, s, path, map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", second.Code)
	}
}

// A renamed screenshot would otherwise show up as a broken image on a page nobody on the
// team looks at twice.
func TestEveryReferencedScreenshotExists(t *testing.T) {
	for _, p := range platforms {
		for _, shot := range p.Screenshots {
			if _, ok := assetNames[shot+".webp"]; !ok {
				t.Errorf("platform %q wants %s.webp, which is not embedded", p.ID, shot)
			}
		}
	}
}

// The page reserves shotWidth x shotHeight for every screenshot before it loads. If a
// regenerated image is a different shape, the reserved box is wrong and the layout jumps
// as each one arrives.
func TestScreenshotsMatchDeclaredSize(t *testing.T) {
	for _, shot := range hiddifyShots {
		t.Run(shot, func(t *testing.T) {
			a, ok := assets[assetNames[shot+".webp"]]
			if !ok {
				t.Fatalf("%s.webp is not embedded", shot)
			}
			w, h, err := webpSize(a.body)
			if err != nil {
				t.Fatalf("%s.webp: %v", shot, err)
			}
			if w != shotWidth || h != shotHeight {
				t.Errorf("%s.webp is %dx%d, but the page reserves %dx%d",
					shot, w, h, shotWidth, shotHeight)
			}
		})
	}
}

// webpSize reads the dimensions out of a simple lossy WebP, which is what cwebp writes
// for these. Only the header, so there is no image decoder and no new dependency.
func webpSize(b []byte) (int, int, error) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, errNotWebP
	}
	if string(b[12:16]) != "VP8 " {
		return 0, 0, fmt.Errorf("chunk %q is not the simple lossy format this reader handles", b[12:16])
	}
	// 20: 3-byte frame tag, then the 3-byte start code, then two 16-bit sizes whose top
	// two bits are a scaling hint.
	if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
		return 0, 0, errNotWebP
	}
	w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
	h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
	return w, h, nil
}

var errNotWebP = errors.New("not a WebP this reader understands")

func TestAssetNamesAreContentAddressed(t *testing.T) {
	for source, hashed := range assetNames {
		if hashed == source {
			t.Errorf("%s was not hashed", source)
		}
		if _, ok := assets[hashed]; !ok {
			t.Errorf("%s maps to %s, which is not served", source, hashed)
		}
	}
}
