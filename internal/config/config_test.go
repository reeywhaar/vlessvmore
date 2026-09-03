package config

import (
	"strings"
	"testing"
	"time"
)

// minimal is the smallest config that should load: everything else defaults.
//
// Note there is no key material here at all — config.json holds no secrets; the Reality
// keypair is generated state and lives in the data directory (see store.Identity).
const minimal = `{
  "host": "vpn.example.test"
}`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"version", cfg.Version, Version},
		{"port", cfg.Port, DefaultPort},
		{"sni falls back to host", cfg.SNI, "vpn.example.test"},
		{"handshake.server falls back to sni", cfg.Handshake.Server, "vpn.example.test"},
		{"handshake.server_port", cfg.Handshake.ServerPort, DefaultHandshakePort},
		{"flow", cfg.FlowValue(), DefaultFlow},
		{"fingerprint", cfg.Fingerprint, DefaultFingerprint},
		{"api_listen", cfg.APIListen, DefaultAPIListen},
		{"backup_url", cfg.BackupURL, ""},
		{"backup_mode", cfg.BackupMode, DefaultBackupMode},
		{"log_level", cfg.LogLevel, DefaultLogLevel},
		{"stats_interval", time.Duration(cfg.StatsInterval), DefaultStatsInterval},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseKeepsExplicitValues(t *testing.T) {
	const in = `{
  "version": 1,
  "host": "vpn.example.test",
  "port": 9443,
  "sni": "other.example",
  "handshake": { "server": "handshake.example.test", "server_port": 8443 },
  "flow": "",
  "fingerprint": "safari",
  "subscription_url_base": "https://sub.example.test",
  "api_listen": "127.0.0.1:8080",
  "backup_url": "http://backup:8080/backup",
  "backup_mode": "all",
  "log_level": "debug",
  "stats_interval": "5s",
  "template": "/etc/vlessvmore/singbox.json.tmpl"
}`
	cfg, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Port != 9443 {
		t.Errorf("port = %d, want 9443", cfg.Port)
	}
	if cfg.SNI != "other.example" {
		t.Errorf("sni = %q, want other.example", cfg.SNI)
	}
	if cfg.Handshake.Server != "handshake.example.test" || cfg.Handshake.ServerPort != 8443 {
		t.Errorf("handshake = %+v", cfg.Handshake)
	}
	if cfg.Fingerprint != "safari" {
		t.Errorf("fingerprint = %q", cfg.Fingerprint)
	}
	if cfg.SubscriptionURLBase != "https://sub.example.test" {
		t.Errorf("subscription_url_base = %q", cfg.SubscriptionURLBase)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q", cfg.LogLevel)
	}
	if time.Duration(cfg.StatsInterval) != 5*time.Second {
		t.Errorf("stats_interval = %s, want 5s", cfg.StatsInterval)
	}
	if cfg.Template != "/etc/vlessvmore/singbox.json.tmpl" {
		t.Errorf("template = %q", cfg.Template)
	}
	if cfg.BackupURL != "http://backup:8080/backup" {
		t.Errorf("backup_url = %q", cfg.BackupURL)
	}
	if cfg.BackupMode != BackupAll {
		t.Errorf("backup_mode = %q, want all", cfg.BackupMode)
	}
	// An explicit empty flow is a legitimate choice (plain vless, no vision), so
	// it must survive rather than being defaulted back to vision — links generated
	// from a silently-restored flow would not connect.
	if cfg.Flow == nil {
		t.Fatal("explicit empty flow became nil")
	}
	if *cfg.Flow != "" {
		t.Errorf("explicit empty flow was overwritten with %q", *cfg.Flow)
	}
	if cfg.FlowValue() != "" {
		t.Errorf("FlowValue() = %q, want empty", cfg.FlowValue())
	}
}

func TestOmittedFlowDefaultsToVision(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.FlowValue() != DefaultFlow {
		t.Errorf("FlowValue() = %q, want %q", cfg.FlowValue(), DefaultFlow)
	}
}

// An omitted backup_url is the off switch, and it has to survive applyDefaults: a default
// here would be a guess at a hostname on a network this program cannot see, and the failure
// it produces is a log line every few minutes about somewhere nobody meant to send anything.
func TestOmittedBackupURLStaysOff(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.BackupURL != "" {
		t.Errorf("backup_url defaulted to %q, want empty", cfg.BackupURL)
	}
	// The mode still gets one, so a deployment that later sets a URL does not also have to
	// discover that it needs a mode.
	if cfg.BackupMode != DefaultBackupMode {
		t.Errorf("backup_mode = %q, want %q", cfg.BackupMode, DefaultBackupMode)
	}
}

// Every mode has to answer both questions the pusher asks of it, and a mode that carries
// nothing or promises a floor it does not keep would be found only at run time.
func TestBackupModes(t *testing.T) {
	tests := []struct {
		mode   BackupMode
		valid  bool
		stats  bool
		period time.Duration
	}{
		{BackupState, true, false, 0},
		{BackupRelaxed, true, true, 0},
		{BackupAll, true, true, BackupAllPeriod},
		{BackupMode("hourly"), false, false, 0},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			if got := tt.mode.Valid(); got != tt.valid {
				t.Errorf("Valid() = %v, want %v", got, tt.valid)
			}
			if got := tt.mode.Stats(); got != tt.stats {
				t.Errorf("Stats() = %v, want %v", got, tt.stats)
			}
			if got := tt.mode.Period(); got != tt.period {
				t.Errorf("Period() = %s, want %s", got, tt.period)
			}
		})
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	const in = `{
  "host": "h",
  "stats_intervall": "30s"
}`
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("Parse accepted an unknown field; a typo must fail loudly")
	}
	if !strings.Contains(err.Error(), "stats_intervall") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestParseRejectsTrailingData(t *testing.T) {
	if _, err := Parse(strings.NewReader(minimal + "\n{}")); err == nil {
		t.Fatal("Parse accepted trailing data after the object")
	}
}

func TestValidateRejectsBadConfigs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"missing host", `{}`, "host is required"},
		{"port out of range", `{"host":"h","port":70000}`, "port"},
		{"bad log level", `{"host":"h","log_level":"verbose"}`, "log_level"},
		{"unsupported version", `{"version":99,"host":"h"}`, "version"},
		{"negative stats interval", `{"host":"h","stats_interval":"-5s"}`, "stats_interval"},
		{"api_listen not an address", `{"host":"h","api_listen":"http://x"}`, "api_listen"},
		{"backup_url has no scheme", `{"host":"h","backup_url":"backup:8080/backup"}`, "backup_url"},
		{"backup_url wrong scheme", `{"host":"h","backup_url":"ftp://backup/backup"}`, "backup_url"},
		{"backup_url has no host", `{"host":"h","backup_url":"http:///backup"}`, "backup_url"},
		{"unknown backup mode", `{"host":"h","backup_mode":"hourly"}`, "backup_mode"},
		{"subscription base has no scheme", `{"host":"h","subscription_url_base":"vpn.example.test"}`, "subscription_url_base"},
		{"subscription base wrong scheme", `{"host":"h","subscription_url_base":"ftp://vpn.example.test"}`, "subscription_url_base"},
		{"subscription base has no host", `{"host":"h","subscription_url_base":"https://"}`, "subscription_url_base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.in))
			if err == nil {
				t.Fatalf("Parse accepted an invalid config")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should mention %q", err, tt.want)
			}
		})
	}
}

// config.json must not be able to carry key material even if someone tries: a stale file
// with a reality block should fail loudly rather than have its keys silently ignored
// while a fresh identity is generated behind its back.
func TestParseRejectsLegacyRealityBlock(t *testing.T) {
	const in = `{"host":"h","reality":{"private_key":"x","short_id":"ab"}}`
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("Parse accepted a legacy reality block")
	}
	if !strings.Contains(err.Error(), "reality") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestSubscriptionBase(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`{"host":"vpn.example.test"}`, "https://vpn.example.test"},
		{`{"host":"vpn.example.test","subscription_url_base":"https://sub.example.test"}`, "https://sub.example.test"},
		{`{"host":"vpn.example.test","subscription_url_base":"https://sub.example.test/"}`, "https://sub.example.test"},
		{`{"host":"vpn.example.test","subscription_url_base":"http://10.0.0.1:8080"}`, "http://10.0.0.1:8080"},
	}
	for _, tt := range tests {
		cfg, err := Parse(strings.NewReader(tt.in))
		if err != nil {
			t.Fatalf("Parse(%s): %v", tt.in, err)
		}
		if got := cfg.SubscriptionBase(); got != tt.want {
			t.Errorf("SubscriptionBase() = %q, want %q", got, tt.want)
		}
	}
}

func TestConfigHoldsNoSecrets(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal))
	if err != nil {
		t.Fatal(err)
	}
	b, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// The whole point of moving key material out is that this file can be committed.
	for _, forbidden := range []string{"private_key", "reality", "bootstrap_token", "secret"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("marshalled config contains %q; config.json must hold no secrets:\n%s", forbidden, b)
		}
	}
}

func TestMarshalRoundTrips(t *testing.T) {
	cfg, err := Parse(strings.NewReader(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// The whole point of Marshal is that `init > config.json` produces a file that
	// Parse accepts, including DisallowUnknownFields.
	got, err := Parse(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("re-parsing marshalled config: %v\n%s", err, b)
	}
	// Compare the re-marshalled bytes rather than the structs: Config has a pointer
	// field, so == would compare addresses instead of values.
	b2, err := got.Marshal()
	if err != nil {
		t.Fatalf("Marshal (second pass): %v", err)
	}
	if string(b2) != string(b) {
		t.Errorf("round trip changed the config:\n got %s\nwant %s", b2, b)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Error("marshalled config should end with a newline")
	}
	if !strings.Contains(string(b), `"stats_interval": "30s"`) {
		t.Errorf("stats_interval should marshal as a readable string, got:\n%s", b)
	}
	if strings.Contains(string(b), "public_key") {
		t.Error("public_key must not be stored in config.json; it is derived")
	}
}

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{`"30s"`, 30 * time.Second, true},
		{`"1m30s"`, 90 * time.Second, true},
		{`45`, 45 * time.Second, true},
		{`0.5`, 500 * time.Millisecond, true},
		{`"nope"`, 0, false},
		{`{}`, 0, false},
	}
	for _, tt := range tests {
		var d Duration
		err := d.UnmarshalJSON([]byte(tt.in))
		if tt.ok {
			if err != nil {
				t.Errorf("UnmarshalJSON(%s): %v", tt.in, err)
			} else if time.Duration(d) != tt.want {
				t.Errorf("UnmarshalJSON(%s) = %s, want %s", tt.in, d, tt.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("UnmarshalJSON(%s) = nil error, want failure", tt.in)
		}
	}
}

func TestLoadMissingFileExplainsInit(t *testing.T) {
	_, err := Load("/nonexistent/vlessvmore/config.json")
	if err == nil {
		t.Fatal("Load succeeded on a missing file")
	}
	// The message is the operator's first contact with the tool; it must say what
	// to run rather than just "no such file or directory".
	if !strings.Contains(err.Error(), "vlessvmore init") {
		t.Errorf("error should tell the operator to run init, got: %v", err)
	}
}

func TestClientLabel(t *testing.T) {
	tests := []struct {
		in       string
		userName string
		want     string
	}{
		{`{"host":"h"}`, "alice", "alice"},
		{`{"host":"h","name":"Reey VPN"}`, "alice", "Reey VPN"},
		// Whitespace-only is the same as unset: it would render as a blank label.
		{`{"host":"h","name":"   "}`, "alice", "alice"},
		{`{"host":"h","name":"  Reey VPN  "}`, "alice", "Reey VPN"},
	}
	for _, tt := range tests {
		cfg, err := Parse(strings.NewReader(tt.in))
		if err != nil {
			t.Fatalf("Parse(%s): %v", tt.in, err)
		}
		if got := cfg.ClientLabel(tt.userName); got != tt.want {
			t.Errorf("ClientLabel for %s = %q, want %q", tt.in, got, tt.want)
		}
	}
}
