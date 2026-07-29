// Package config models *our* config.json — not sing-box's.
//
// This file is the operator's only hand-authored artifact: where the server lives and
// how it behaves. It is mounted read-only and nothing at runtime writes to it.
//
// It deliberately contains **no secrets**. The Reality keypair is generated state and
// lives in the data directory (see store.Identity); API tokens live there too. That
// makes config.json safe to commit, template or hand around, and means losing it costs
// nothing but a retype.
//
// The sing-box config is a third, separate thing, rendered from this plus the identity
// and the user list (see internal/singbox).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is the current config.json format version. Bumping it lets a future
// release recognise and migrate old files instead of misreading them.
const Version = 1

// DefaultPath is where serve looks for config.json inside the container.
const DefaultPath = "/etc/vlessvmore/config.json"

// PathEnv overrides DefaultPath.
const PathEnv = "VLESSVMORE_CONFIG"

// Defaults applied when a field is omitted.
const (
	DefaultPort          = 8443
	DefaultFlow          = "xtls-rprx-vision"
	DefaultFingerprint   = "chrome"
	DefaultAPIListen     = ":80"
	DefaultBackupListen  = ":3000"
	DefaultLogLevel      = "info"
	DefaultStatsInterval = 30 * time.Second
	DefaultHandshakePort = 443
)

// Config is the parsed config.json.
type Config struct {
	Version int `json:"version"`

	// Name is what clients display for this server: the `#fragment` of a vless:// URI
	// and the subscription's Profile-Title. Optional; when unset, clients fall back to
	// showing the user's own name, which reads oddly — Alice does not need to be told
	// she is Alice.
	//
	// Deliberately not called server_name: that is what sing-box calls the Reality SNI,
	// and having two unrelated fields by that name in adjacent files would be a trap.
	Name string `json:"name,omitempty"`

	Host string `json:"host"`
	Port int    `json:"port"`
	SNI  string `json:"sni"`

	Handshake Handshake `json:"handshake"`

	// Flow is a pointer so that omitted and explicitly-empty are distinguishable.
	// sing-box vless accepts exactly two states — absent, or "xtls-rprx-vision" —
	// and an operator who writes "flow": "" wants plain vless. Defaulting that back
	// to vision would hand out links whose clients cannot connect, so nil means
	// "not set, use the default" and "" means "deliberately no flow".
	Flow        *string `json:"flow"`
	Fingerprint string  `json:"fingerprint"`

	// SubscriptionURLBase is the public origin clients reach /sub/<token> on, e.g.
	// "https://vpn.example.com". Defaults to https://<host>, which is correct for the
	// recommended topology where a reverse proxy fronts this service on the same
	// hostname Reality uses. Set it when the API lives somewhere else.
	SubscriptionURLBase string `json:"subscription_url_base,omitempty"`

	APIListen string `json:"api_listen"`

	// BackupListen is where GET /backup serves a tgz of the whole deployment. A pointer
	// for the same reason as Flow: nil means "omitted, use the default" and "" means
	// "deliberately off".
	//
	// A port of its own rather than a route on api_listen, because api_listen is what a
	// reverse proxy fronts on the public hostname while this endpoint hands out the
	// Reality private key and every user UUID with no bearer token. Not being published
	// is its only protection — reachable from a sibling container and nowhere else.
	BackupListen *string `json:"backup_listen"`

	LogLevel      string   `json:"log_level"`
	StatsInterval Duration `json:"stats_interval"`

	// CORSOrigins lists the origins allowed to call /api from a browser, e.g.
	// ["https://dash.example.com"]. A single "*" allows any.
	//
	// Empty — the default — means no cross-origin access, and is the right setting
	// unless something actually needs it. A CORS preflight carries no Authorization
	// header, because browsers never send one, so every preflight this server answers
	// is answered to an unauthenticated stranger. Answering only for paths that exist
	// tells that stranger which paths exist. Listing specific origins keeps that
	// knowledge behind knowing the dashboard's hostname; "*" gives it to everyone.
	CORSOrigins []string `json:"cors_origins,omitempty"`

	// Template optionally points at a mounted sing-box template that replaces the
	// embedded one. Advanced use only; the rendered output is still gated by
	// `sing-box check`, so a broken override cannot take down a running proxy.
	Template string `json:"template,omitempty"`
}

// Handshake is the Reality handshake target — the real TLS server sing-box forwards
// non-authenticated traffic to. It must hold a valid certificate for SNI.
type Handshake struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
}

// normalizeOrigin puts an entry into the exact form a browser sends, so the request-time
// check can stay a plain string match.
//
// Case and a trailing slash are just sloppiness. The default port is the trap: RFC 6454's
// serialization omits it, so a browser on https://dash.example.com sends exactly that and
// never ":443". Written with the port, the entry would be valid, look right, and never
// once match.
func normalizeOrigin(o string) string {
	o = strings.ToLower(strings.TrimRight(strings.TrimSpace(o), "/"))
	switch {
	case strings.HasPrefix(o, "https://"):
		return strings.TrimSuffix(o, ":443")
	case strings.HasPrefix(o, "http://"):
		return strings.TrimSuffix(o, ":80")
	}
	return o
}

// Load reads and validates config.json from path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s: run `vlessvmore init --host <host> > config.json` and mount it there", path)
		}
		return nil, err
	}
	defer f.Close()

	cfg, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a config from r. Unknown fields are rejected so a
// typo fails loudly at startup rather than being silently ignored — a misspelled
// `stats_intervall` that quietly reverts to the default is exactly the kind of bug
// that takes a day to find.
func Parse(r io.Reader) (*Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse config: unexpected trailing data after the JSON object")
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = Version
	}
	for i, o := range c.CORSOrigins {
		c.CORSOrigins[i] = normalizeOrigin(o)
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.SNI == "" {
		c.SNI = c.Host
	}
	if c.Handshake.Server == "" {
		c.Handshake.Server = c.SNI
	}
	if c.Handshake.ServerPort == 0 {
		c.Handshake.ServerPort = DefaultHandshakePort
	}
	if c.Flow == nil {
		flow := DefaultFlow
		c.Flow = &flow
	}
	if c.Fingerprint == "" {
		c.Fingerprint = DefaultFingerprint
	}
	if c.APIListen == "" {
		c.APIListen = DefaultAPIListen
	}
	if c.BackupListen == nil {
		addr := DefaultBackupListen
		c.BackupListen = &addr
	}
	if c.LogLevel == "" {
		c.LogLevel = DefaultLogLevel
	}
	if c.StatsInterval == 0 {
		c.StatsInterval = Duration(DefaultStatsInterval)
	}
}

// Validate reports the first problem that would make this config unusable.
func (c *Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("unsupported config version %d, this build understands %d", c.Version, Version)
	}
	if c.Host == "" {
		return errors.New("host is required: the public hostname clients dial")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", c.Port)
	}
	if c.Handshake.ServerPort < 1 || c.Handshake.ServerPort > 65535 {
		return fmt.Errorf("handshake.server_port %d out of range 1-65535", c.Handshake.ServerPort)
	}
	if c.SubscriptionURLBase != "" {
		u, err := url.Parse(c.SubscriptionURLBase)
		if err != nil {
			return fmt.Errorf("subscription_url_base %q is not a URL: %w", c.SubscriptionURLBase, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("subscription_url_base %q must start with http:// or https://", c.SubscriptionURLBase)
		}
		if u.Host == "" {
			return fmt.Errorf("subscription_url_base %q has no host", c.SubscriptionURLBase)
		}
	}
	for _, o := range c.CORSOrigins {
		if o == "*" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("cors_origins %q must be an origin like https://dash.example.com, or \"*\"", o)
		}
		// An Origin header is scheme://host[:port] and nothing else, so anything with a
		// path will simply never match what a browser sends.
		if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("cors_origins %q must be scheme://host[:port] with no path", o)
		}
	}
	switch c.LogLevel {
	case "trace", "debug", "info", "warn", "error", "fatal", "panic":
	default:
		return fmt.Errorf("log_level %q is not a sing-box level (trace debug info warn error fatal panic)", c.LogLevel)
	}
	if c.StatsInterval <= 0 {
		return fmt.Errorf("stats_interval must be positive, got %s", time.Duration(c.StatsInterval))
	}
	apiPort, err := listenPort("api_listen", c.APIListen)
	if err != nil {
		return err
	}
	if backup := c.BackupListenValue(); backup != "" {
		backupPort, err := listenPort("backup_listen", backup)
		if err != nil {
			return err
		}
		// Both would bind, one would lose, and the loser's failure arrives as a bare
		// "address already in use" at startup.
		if backupPort == apiPort {
			return fmt.Errorf("backup_listen %q and api_listen %q are the same port", backup, c.APIListen)
		}
	}
	return nil
}

// listenPort validates a listen address and returns its port.
func listenPort(field, addr string) (int, error) {
	_, port, err := net.SplitHostPort(withDefaultHost(addr))
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a host:port address: %w", field, addr, err)
	}
	// SplitHostPort happily accepts "http://x" as host "http", port "//x", so the
	// port has to be range-checked separately.
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%s %q: port must be a number in 1-65535, got %q", field, addr, port)
	}
	return n, nil
}

// FlowValue is the vless flow to use, "" meaning none.
func (c *Config) FlowValue() string {
	if c.Flow == nil {
		return DefaultFlow
	}
	return *c.Flow
}

// BackupListenValue is the address to serve /backup on, "" meaning do not serve it.
func (c *Config) BackupListenValue() string {
	if c.BackupListen == nil {
		return DefaultBackupListen
	}
	return *c.BackupListen
}

// withDefaultHost lets ":80" validate as an address.
func withDefaultHost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "0.0.0.0" + addr
	}
	return addr
}

// ClientLabel is what a client should display for this profile, falling back to the
// user's own name when no server name is configured.
func (c *Config) ClientLabel(userName string) string {
	if c.Name != "" {
		return c.Name
	}
	return userName
}

// SubscriptionBase is the origin to build subscription URLs on.
func (c *Config) SubscriptionBase() string {
	if c.SubscriptionURLBase != "" {
		return strings.TrimRight(c.SubscriptionURLBase, "/")
	}
	// https, not http: clients refuse plaintext subscription URLs, and the documented
	// topology terminates TLS for this hostname anyway.
	return "https://" + c.Host
}

// Marshal renders the config as the indented JSON that `init` writes to stdout.
func (c *Config) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Duration is a time.Duration that marshals as a string like "30s", so
// config.json stays human-editable instead of carrying raw nanoseconds.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	// Accept a plain number as seconds too; it is a natural thing to write.
	if n, err := strconv.ParseFloat(string(b), 64); err == nil {
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or a number of seconds: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }
