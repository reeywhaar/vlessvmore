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

// BackupMode is what goes into a backup archive, and what makes one happen.
//
// Two questions with three useful answers between them, rather than two switches with a
// meaningless fourth combination. "stats only, when a user changes" is not a policy anybody
// wants, and a pair of booleans would offer it.
type BackupMode string

const (
	// BackupState carries the JSON files alone: the Reality keypair, the users, the API
	// token hashes and the config the server was started with.
	//
	// The smallest thing worth keeping, and the part that cannot be rebuilt from anywhere.
	BackupState BackupMode = "state"

	// BackupRelaxed carries stats.db too, and is the default.
	//
	// Traffic history comes along for the ride but never decides that a copy is due. What
	// it holds is measurement rather than intent: an instance restored without it forgets
	// how much everybody has used this month, which resets quota windows and loses a graph,
	// but hands out no broken links. Cheap enough to carry, so carried; not worth waking up
	// for, so it wakes nothing.
	BackupRelaxed BackupMode = "relaxed"

	// BackupAll is relaxed with a floor under it: a copy at least every [BackupAllPeriod],
	// whether or not anybody touched a user.
	//
	// The one mode that sends an archive when nothing a person did has changed. Traffic is
	// the thing the change check never sees — a server nobody administers for a week still
	// meters every byte through it, and to a check that hashes the JSON files that week
	// looks exactly like an idle one. For an operator who wants usage kept closely rather
	// than as of the last time somebody added a user.
	BackupAll BackupMode = "all"
)

// How long a copy waits, and how long a deployment can go without one.
//
// Constants, not settings, and deliberately. Neither is a number an operator can reason
// about better than this program can: the delay trades "how much of a burst becomes one
// archive" against "how long a change sits uncopied", and the floor exists only to catch
// traffic, which the change check never sees. Both have one right answer on every
// deployment this runs on, and a knob would mostly be a way to set them wrong.
//
// What an operator actually chooses is which of those promises they want, and that is
// backup_mode.
const (
	// BackupDelay is how long after a change the copy goes out, in every mode.
	//
	// A delay and a throttle at once: nothing here reacts to a write, so an operator adding
	// six users in a minute gets one archive holding all six rather than six archives.
	BackupDelay = 5 * time.Minute

	// BackupAllPeriod is how long [BackupAll] will go without sending anything.
	BackupAllPeriod = 30 * time.Minute
)

// Valid reports whether m is a mode this build knows.
func (m BackupMode) Valid() bool {
	return m == BackupState || m == BackupRelaxed || m == BackupAll
}

// Stats reports whether the archive carries stats.db.
func (m BackupMode) Stats() bool { return m == BackupRelaxed || m == BackupAll }

// Period is how long a mode will go without sending anything, or zero for no floor at all.
//
// Only [BackupAll] has one. Every mode sends when the deployment changes; this is the extra
// promise one of them makes on top.
func (m BackupMode) Period() time.Duration {
	if m == BackupAll {
		return BackupAllPeriod
	}
	return 0
}

// Defaults applied when a field is omitted.
const (
	DefaultPort          = 8443
	DefaultFlow          = "xtls-rprx-vision"
	DefaultFingerprint   = "chrome"
	DefaultAPIListen     = ":80"
	DefaultLogLevel      = "info"
	DefaultStatsInterval = 30 * time.Second
	DefaultHandshakePort = 443

	// DefaultBackupMode carries every file and copies them when a user, token, key or the
	// config changes.
	//
	// Relaxed rather than state, because stats.db is small next to the reason anybody runs
	// this — a year of hourly buckets for fifty users is a couple of megabytes — and an
	// archive without it restores a server whose quota windows have all silently reset.
	DefaultBackupMode = BackupRelaxed
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

	// BackupURL is where an archive is posted, or empty for no backups at all — a backup
	// agent's endpoint, so on a compose network something like "http://backup:8080/backup".
	//
	// The whole address rather than a host, because the endpoint is the agent's to name and
	// this program should not be the place that knows the path it happens to serve today.
	//
	// Nothing is backed up unless this is set. There is no default, because a default would
	// be a guess at a hostname on a network this program cannot see, and the failure it
	// produces is a log line every few minutes about somewhere nobody meant to send
	// anything.
	//
	// Not a secret, which is what keeps it in this file: the credential for the remote
	// belongs to the agent, and this end holds nothing but an address on a private network.
	BackupURL string `json:"backup_url,omitempty"`

	// BackupMode is what the archive carries, and what makes one happen. See [BackupMode].
	BackupMode BackupMode `json:"backup_mode,omitempty"`

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
	if c.BackupMode == "" {
		c.BackupMode = DefaultBackupMode
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
	if _, err := listenPort("api_listen", c.APIListen); err != nil {
		return err
	}
	if !c.BackupMode.Valid() {
		return fmt.Errorf("backup_mode %q is not one of state, relaxed or all", c.BackupMode)
	}
	if c.BackupURL != "" {
		u, err := url.Parse(c.BackupURL)
		if err != nil {
			return fmt.Errorf("backup_url %q is not a URL: %w", c.BackupURL, err)
		}
		// The scheme is required rather than assumed. A bare "backup:8080/backup" parses
		// as a relative path with no host at all, and the request that produces fails at
		// the first push rather than at startup, which is the wrong end of the day to find
		// out that nothing has ever been backed up.
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("backup_url %q must start with http:// or https://", c.BackupURL)
		}
		if u.Host == "" {
			return fmt.Errorf("backup_url %q has no host", c.BackupURL)
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
