package singbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"vlessvmore/internal/config"
	"vlessvmore/internal/store"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	const in = `{
  "host": "vpn.example.test",
  "port": 8443,
  "handshake": { "server": "handshake.example.test", "server_port": 443 }
}`
	cfg, err := config.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse test config: %v", err)
	}
	return cfg
}

// testIdentity is the RFC 7748 Alice vector, used so the rendered private key is a known
// published value rather than anyone's real key.
func testIdentity() store.Identity {
	return store.Identity{
		Version:    1,
		PrivateKey: "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCo",
		ShortID:    "0102030405060708",
	}
}

func testUsers() []store.User {
	return []store.User{
		{ID: "u_AAAAAAAAAAAAAAAAAAAAAAAAAA", Name: "alice", UUID: "11111111-2222-3333-4444-555555555555"},
		{ID: "u_BBBBBBBBBBBBBBBBBBBBBBBBBB", Name: "bob", UUID: "66666666-7777-8888-9999-000000000000"},
	}
}

func render(t *testing.T, cfg *config.Config, users []store.User) map[string]any {
	t.Helper()
	r, err := NewRenderer(cfg)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	doc, err := r.Render(cfg, testIdentity(), users)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(doc, &out); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v\n%s", err, doc)
	}
	return out
}

// The rendered document must reproduce the known-good production config. These are
// the fields that are load-bearing but easy to lose silently in a refactor: the
// ipv4-only resolver and the sniff/resolve rules do not stop the proxy from working
// if dropped, they just make it resolve badly.
func TestRenderGolden(t *testing.T) {
	cfg := testConfig(t)
	got := render(t, cfg, testUsers())

	route, ok := got["route"].(map[string]any)
	if !ok {
		t.Fatalf("no route block: %v", got)
	}
	resolver, ok := route["default_domain_resolver"].(map[string]any)
	if !ok {
		t.Fatalf("no default_domain_resolver: %v", route)
	}
	if resolver["server"] != "local" {
		t.Errorf("resolver server = %v, want local", resolver["server"])
	}
	if resolver["strategy"] != "ipv4_only" {
		t.Errorf("resolver strategy = %v, want ipv4_only", resolver["strategy"])
	}

	rules, ok := route["rules"].([]any)
	if !ok || len(rules) != 2 {
		t.Fatalf("route.rules = %v, want 2 rules", route["rules"])
	}
	if r0 := rules[0].(map[string]any); r0["action"] != "sniff" {
		t.Errorf("rules[0] = %v, want sniff", r0)
	}
	r1 := rules[1].(map[string]any)
	if r1["action"] != "resolve" || r1["strategy"] != "ipv4_only" {
		t.Errorf("rules[1] = %v, want resolve/ipv4_only", r1)
	}

	dns, ok := got["dns"].(map[string]any)
	if !ok {
		t.Fatalf("no dns block")
	}
	servers := dns["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("dns.servers = %v, want 1", servers)
	}
	s0 := servers[0].(map[string]any)
	if s0["type"] != "local" || s0["tag"] != "local" {
		t.Errorf("dns server = %v, want local/local", s0)
	}

	outbounds := got["outbounds"].([]any)
	if len(outbounds) != 1 || outbounds[0].(map[string]any)["type"] != "direct" {
		t.Errorf("outbounds = %v, want one direct", outbounds)
	}
}

func TestRenderInboundFromConfig(t *testing.T) {
	cfg := testConfig(t)
	got := render(t, cfg, testUsers())

	inbounds := got["inbounds"].([]any)
	if len(inbounds) != 1 {
		t.Fatalf("inbounds = %d, want 1", len(inbounds))
	}
	in := inbounds[0].(map[string]any)

	if in["type"] != "vless" {
		t.Errorf("type = %v, want vless", in["type"])
	}
	if in["tag"] != InboundTag {
		t.Errorf("tag = %v, want %s", in["tag"], InboundTag)
	}
	// "::" so the inbound accepts both v4 and v6 clients.
	if in["listen"] != "::" {
		t.Errorf("listen = %v, want ::", in["listen"])
	}
	if in["listen_port"].(float64) != 8443 {
		t.Errorf("listen_port = %v, want 8443", in["listen_port"])
	}

	tls := in["tls"].(map[string]any)
	if tls["enabled"] != true {
		t.Error("tls not enabled")
	}
	if tls["server_name"] != "vpn.example.test" {
		t.Errorf("server_name = %v", tls["server_name"])
	}

	r := tls["reality"].(map[string]any)
	if r["enabled"] != true {
		t.Error("reality not enabled")
	}
	id := testIdentity()
	if r["private_key"] != id.PrivateKey {
		t.Errorf("private_key = %v, want the identity's key", r["private_key"])
	}
	// short_id is an array in sing-box even when there is one of them.
	sids := r["short_id"].([]any)
	if len(sids) != 1 || sids[0] != id.ShortID {
		t.Errorf("short_id = %v, want [%s]", sids, id.ShortID)
	}
	hs := r["handshake"].(map[string]any)
	if hs["server"] != "handshake.example.test" || hs["server_port"].(float64) != 443 {
		t.Errorf("handshake = %v", hs)
	}
}

// A user present in the inbound but missing from stats.users connects normally and is
// never metered — a failure that looks like a broken collector. Deriving both lists in
// one pass is what prevents it, so this asserts they match exactly.
func TestRenderStatsUsersMatchInboundUsers(t *testing.T) {
	cfg := testConfig(t)
	got := render(t, cfg, testUsers())

	in := got["inbounds"].([]any)[0].(map[string]any)
	var inboundIDs []string
	for _, u := range in["users"].([]any) {
		inboundIDs = append(inboundIDs, u.(map[string]any)["name"].(string))
	}

	stats := got["experimental"].(map[string]any)["v2ray_api"].(map[string]any)["stats"].(map[string]any)
	var statsIDs []string
	for _, id := range stats["users"].([]any) {
		statsIDs = append(statsIDs, id.(string))
	}

	slices.Sort(inboundIDs)
	slices.Sort(statsIDs)
	if !slices.Equal(inboundIDs, statsIDs) {
		t.Errorf("stats.users and inbound users differ:\n inbound: %v\n   stats: %v", inboundIDs, statsIDs)
	}
	if len(inboundIDs) != 2 {
		t.Errorf("expected 2 users, got %v", inboundIDs)
	}
}

// sing-box's user `name` is what stats are keyed on. It must be the internal id, not
// the display name, or renaming a user silently orphans their history.
func TestRenderUsesInternalIDNotDisplayName(t *testing.T) {
	cfg := testConfig(t)
	users := testUsers()
	got := render(t, cfg, users)

	in := got["inbounds"].([]any)[0].(map[string]any)
	entries := in["users"].([]any)
	first := entries[0].(map[string]any)

	if first["name"] != users[0].ID {
		t.Errorf("user name = %v, want the internal id %s", first["name"], users[0].ID)
	}
	if first["name"] == users[0].Name {
		t.Error("user name is the display name; renaming would orphan usage counters")
	}
	if first["uuid"] != users[0].UUID {
		t.Errorf("uuid = %v, want %s", first["uuid"], users[0].UUID)
	}
	if first["flow"] != config.DefaultFlow {
		t.Errorf("flow = %v, want %s", first["flow"], config.DefaultFlow)
	}
}

func TestRenderOmitsFlowWhenUnset(t *testing.T) {
	cfg := testConfig(t)
	empty := ""
	cfg.Flow = &empty

	got := render(t, cfg, testUsers())
	in := got["inbounds"].([]any)[0].(map[string]any)
	first := in["users"].([]any)[0].(map[string]any)

	// An empty flow must be absent, not present-and-empty: sing-box would reject "".
	if _, present := first["flow"]; present {
		t.Errorf("flow should be omitted entirely when unset, got %v", first["flow"])
	}
}

func TestRenderNoUsers(t *testing.T) {
	cfg := testConfig(t)
	got := render(t, cfg, nil)

	in := got["inbounds"].([]any)[0].(map[string]any)
	users, ok := in["users"].([]any)
	if !ok {
		t.Fatalf("users = %#v, want an array", in["users"])
	}
	// An empty array, never null: sing-box accepts [] and would fail on null.
	if len(users) != 0 {
		t.Errorf("users = %v, want empty", users)
	}

	stats := got["experimental"].(map[string]any)["v2ray_api"].(map[string]any)["stats"].(map[string]any)
	if got := stats["users"].([]any); len(got) != 0 {
		t.Errorf("stats.users = %v, want empty", got)
	}
}

func TestRenderV2RayAPIOnLoopback(t *testing.T) {
	cfg := testConfig(t)
	got := render(t, cfg, testUsers())

	api := got["experimental"].(map[string]any)["v2ray_api"].(map[string]any)
	listen, _ := api["listen"].(string)
	// The gRPC stats service is unauthenticated; exposing it off-loopback would hand
	// out every user's traffic to anyone who can reach the port.
	if !strings.HasPrefix(listen, "127.0.0.1:") {
		t.Errorf("v2ray_api listen = %q, must be loopback-only", listen)
	}
	if api["stats"].(map[string]any)["enabled"] != true {
		t.Error("stats not enabled; no per-user counters would exist")
	}
	inbounds := api["stats"].(map[string]any)["inbounds"].([]any)
	if len(inbounds) != 1 || inbounds[0] != InboundTag {
		t.Errorf("stats.inbounds = %v, want [%s]", inbounds, InboundTag)
	}
}

// Values reach the document through a json helper rather than bare interpolation, so
// a hostname containing a quote cannot produce a broken config.
func TestRenderEscapesHostileValues(t *testing.T) {
	cfg := testConfig(t)
	cfg.SNI = `evil".example/\test`
	cfg.Handshake.Server = "back\\slash"

	got := render(t, cfg, testUsers())
	tls := got["inbounds"].([]any)[0].(map[string]any)["tls"].(map[string]any)
	if tls["server_name"] != cfg.SNI {
		t.Errorf("server_name = %q, want %q", tls["server_name"], cfg.SNI)
	}
	hs := tls["reality"].(map[string]any)["handshake"].(map[string]any)
	if hs["server"] != cfg.Handshake.Server {
		t.Errorf("handshake server = %q, want %q", hs["server"], cfg.Handshake.Server)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	cfg := testConfig(t)
	r, err := NewRenderer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Render(cfg, testIdentity(), testUsers())
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := r.Render(cfg, testIdentity(), testUsers())
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatal("Render is not deterministic for identical input")
		}
	}
}

func TestRenderTemplateOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json.tmpl")
	if err := os.WriteFile(path, []byte(`{"marker": {{json .Config.SNI}}, "port": {{json .Config.Port}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	cfg.Template = path
	got := render(t, cfg, testUsers())

	if got["marker"] != "vpn.example.test" {
		t.Errorf("override template not used: %v", got)
	}
}

func TestRenderRejectsBrokenOverride(t *testing.T) {
	dir := t.TempDir()

	// Invalid JSON output must be caught here, with a clear message, rather than
	// surfacing later as an opaque `sing-box check` failure.
	broken := filepath.Join(dir, "broken.json.tmpl")
	if err := os.WriteFile(broken, []byte(`{"unclosed": `), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Template = broken
	r, err := NewRenderer(cfg)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if _, err := r.Render(cfg, testIdentity(), testUsers()); err == nil {
		t.Error("Render accepted a template producing invalid JSON")
	}

	// A template that will not even parse must fail at load time.
	unparseable := filepath.Join(dir, "bad.json.tmpl")
	if err := os.WriteFile(unparseable, []byte(`{{ if }}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Template = unparseable
	if _, err := NewRenderer(cfg); err == nil {
		t.Error("NewRenderer accepted an unparseable template")
	}

	cfg.Template = filepath.Join(dir, "missing.tmpl")
	if _, err := NewRenderer(cfg); err == nil {
		t.Error("NewRenderer accepted a missing template path")
	}
}
