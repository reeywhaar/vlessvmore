package api

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed web/static
var staticFS embed.FS

// StaticPath serves the install page's assets. A subscription token is required, so
// /static/app.js on its own is a 404 like any other unregistered path.
const StaticPath = "/static/"

// asset is one embedded file, named by its content.
type asset struct {
	name  string // As requested: app.<hash>.css
	body  []byte
	etag  string
	ctype string
}

// assets is keyed by the hashed name. Built at init; the set never changes at runtime.
var assets = mustLoadAssets()

// assetNames maps a source name ("app.css") to its hashed name, for the template.
var assetNames = map[string]string{}

func mustLoadAssets() map[string]asset {
	out, err := loadAssets()
	if err != nil {
		panic(err)
	}
	return out
}

func loadAssets() (map[string]asset, error) {
	out := map[string]asset{}
	err := fs.WalkDir(staticFS, "web/static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		ctype, ok := assetType(path.Ext(p))
		if !ok {
			return fmt.Errorf("asset %s has no known content type", p)
		}

		// Content-addressed, so the response can be cached indefinitely and an edit to
		// the file changes the URL rather than needing anyone to think about expiry.
		sum := sha256.Sum256(body)
		short := hex.EncodeToString(sum[:4])
		base := path.Base(p)
		ext := path.Ext(base)
		hashed := strings.TrimSuffix(base, ext) + "." + short + ext

		out[hashed] = asset{
			name:  hashed,
			body:  body,
			etag:  `"` + short + `"`,
			ctype: ctype,
		}
		assetNames[base] = hashed
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no assets embedded")
	}
	return out, nil
}

// assetType maps an extension to a content type. An explicit table rather than
// mime.TypeByExtension, whose answer depends on the host's /etc/mime.types.
func assetType(ext string) (string, bool) {
	switch ext {
	case ".css":
		return "text/css; charset=utf-8", true
	case ".js":
		return "text/javascript; charset=utf-8", true
	case ".webp":
		return "image/webp", true
	case ".svg":
		return "image/svg+xml", true
	}
	return "", false
}

// assetURL is the path the page should reference for a source file name.
func assetURL(name, token string) string {
	hashed, ok := assetNames[name]
	if !ok {
		// A typo in the template. Returning an obviously broken path beats serving a
		// page that silently has no styling.
		return StaticPath + "missing/" + name
	}
	return StaticPath + hashed + "?token=" + token
}

// staticAsset serves an embedded file to anyone holding a subscription token.
//
// The gate is the same capability that opens the install page itself, which keeps the
// assets from being a second, unauthenticated way to learn this host is more than a
// static site.
func (s *Server) staticAsset(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.Users.GetBySubToken(r.URL.Query().Get("token")); err != nil {
		s.notFound(w, r)
		return
	}
	a, ok := assets[r.PathValue("name")]
	if !ok {
		s.notFound(w, r)
		return
	}

	w.Header().Set("Content-Type", a.ctype)
	w.Header().Set("ETag", a.etag)
	// private: the URL carries a credential, so a shared cache has no business keeping a
	// copy. immutable: the name changes when the bytes do.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

	if match := r.Header.Get("If-None-Match"); match == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.body)
}
