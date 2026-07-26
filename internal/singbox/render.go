// Package singbox renders the sing-box configuration and supervises the process.
//
// The rendered config is a build artifact of (config.json + the active user list),
// not state. It is regenerated from scratch on every start and after every change,
// never patched in place, so there is exactly one code path that can produce a
// sing-box config and exactly one thing to test.
package singbox

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"text/template"

	"vlessvmore/internal/config"
	"vlessvmore/internal/store"
)

//go:embed singbox.json.tmpl
var embedded embed.FS

// InboundTag is the tag of the vless inbound we manage. It also appears in
// v2ray_api's inbound stats list.
const InboundTag = "vless-in"

// StatsListen is where sing-box exposes the v2ray_api gRPC service. Loopback only:
// it is an unauthenticated API and only the manager in the same container calls it.
const StatsListen = "127.0.0.1:8081"

// templateUser is one entry of the inbound's `users` array.
//
// Name is the user's internal id, not their display name. sing-box only uses it for
// stats attribution, and keying stats on a mutable display name would lose a user's
// history the moment they were renamed.
type templateUser struct {
	Name string `json:"name"`
	UUID string `json:"uuid"`
	Flow string `json:"flow,omitempty"`
}

// templateData is what the template sees.
type templateData struct {
	Config      *config.Config
	Identity    store.Identity
	Users       []templateUser
	UserIDs     []string
	InboundTag  string
	StatsListen string
}

// funcs gives the template a `json` helper.
//
// Every interpolated value goes through it rather than being written as a bare
// "{{.Field}}" inside quotes. That handles escaping — a hostname or note containing
// a quote or backslash would otherwise produce invalid JSON — and renders slices
// without the trailing-comma bugs that hand-written {{range}} loops attract.
var funcs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
}

// Renderer turns config plus users into a sing-box config document.
type Renderer struct {
	tmpl *template.Template
	// path is the override template, empty when using the embedded one. Recorded for
	// error messages only.
	path string
}

// NewRenderer loads the embedded template, or an override from disk when
// cfg.Template is set.
//
// The override exists as an escape hatch for setups the embedded template cannot
// express (an extra inbound, different DNS). It is deliberately unadvertised: the
// output is still gated by `sing-box check`, so a broken override cannot take down a
// running proxy, but nothing else about it is supported.
func NewRenderer(cfg *config.Config) (*Renderer, error) {
	if cfg.Template == "" {
		tmpl, err := template.New("singbox.json.tmpl").Funcs(funcs).ParseFS(embedded, "singbox.json.tmpl")
		if err != nil {
			return nil, fmt.Errorf("parse embedded template: %w", err)
		}
		return &Renderer{tmpl: tmpl}, nil
	}

	b, err := os.ReadFile(cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", cfg.Template, err)
	}
	tmpl, err := template.New("override").Funcs(funcs).Parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", cfg.Template, err)
	}
	return &Renderer{tmpl: tmpl, path: cfg.Template}, nil
}

// Render produces the sing-box config for the given active users.
//
// Users and UserIDs are derived from the same slice in the same pass, so the
// inbound's user list and v2ray_api's stats.users can never drift. That matters:
// sing-box silently declines to meter any user missing from stats.users, which would
// look like a client that connects fine but never accrues traffic.
func (r *Renderer) Render(cfg *config.Config, id store.Identity, users []store.User) ([]byte, error) {
	data := templateData{
		Config:      cfg,
		Identity:    id,
		Users:       make([]templateUser, 0, len(users)),
		UserIDs:     make([]string, 0, len(users)),
		InboundTag:  InboundTag,
		StatsListen: StatsListen,
	}
	for _, u := range users {
		data.Users = append(data.Users, templateUser{
			Name: u.ID,
			UUID: u.UUID,
			Flow: cfg.FlowValue(),
		})
		data.UserIDs = append(data.UserIDs, u.ID)
	}

	var buf bytes.Buffer
	if err := r.tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", r.name(), err)
	}

	// Re-encode through encoding/json so the output is canonical and, more
	// importantly, so a template that produced malformed JSON fails here with a
	// precise error rather than as an opaque `sing-box check` failure later.
	var doc any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		return nil, fmt.Errorf("%s produced invalid JSON: %w\n%s", r.name(), err, buf.String())
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func (r *Renderer) name() string {
	if r.path == "" {
		return "embedded template"
	}
	return "template " + r.path
}
