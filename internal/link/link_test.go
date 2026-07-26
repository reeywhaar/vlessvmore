package link

import (
	"net/url"
	"strings"
	"testing"
)

func base() Params {
	return Params{
		UUID:        "11111111-2222-3333-4444-555555555555",
		Host:        "vpn.example.test",
		Port:        8443,
		SNI:         "vpn.example.test",
		PublicKey:   "hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo",
		ShortID:     "0102030405060708",
		Flow:        "xtls-rprx-vision",
		Fingerprint: "chrome",
		Name:        "alice",
	}
}

// The parameter order and spelling must match what the previous deployment
// documented, so operators' existing notes stay correct.
func TestBuildGolden(t *testing.T) {
	const want = "vless://11111111-2222-3333-4444-555555555555@vpn.example.test:8443" +
		"?type=tcp&encryption=none&flow=xtls-rprx-vision&packetEncoding=xudp" +
		"&security=reality&sni=vpn.example.test&fp=chrome" +
		"&pbk=hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo&sid=0102030405060708#alice"

	if got := Build(base()); got != want {
		t.Errorf("Build()\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildOmitsEmptyFlow(t *testing.T) {
	p := base()
	p.Flow = ""
	got := Build(p)
	if strings.Contains(got, "flow=") {
		t.Errorf("empty flow should be omitted entirely, got: %s", got)
	}
	// Everything else must survive the omission.
	if !strings.Contains(got, "security=reality") {
		t.Errorf("missing security param: %s", got)
	}
}

func TestBuildOmitsEmptyFingerprint(t *testing.T) {
	p := base()
	p.Fingerprint = ""
	if got := Build(p); strings.Contains(got, "fp=") {
		t.Errorf("empty fingerprint should be omitted, got: %s", got)
	}
}

func TestBuildParsesAsURL(t *testing.T) {
	u, err := url.Parse(Build(base()))
	if err != nil {
		t.Fatalf("Build produced an unparseable URI: %v", err)
	}
	if u.Scheme != "vless" {
		t.Errorf("scheme = %q, want vless", u.Scheme)
	}
	if u.User.Username() != base().UUID {
		t.Errorf("userinfo = %q, want the uuid", u.User.Username())
	}
	if u.Host != "vpn.example.test:8443" {
		t.Errorf("host = %q", u.Host)
	}
	if u.Fragment != "alice" {
		t.Errorf("fragment = %q, want alice", u.Fragment)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"type":           "tcp",
		"encryption":     "none",
		"flow":           "xtls-rprx-vision",
		"packetEncoding": "xudp",
		"security":       "reality",
		"sni":            "vpn.example.test",
		"fp":             "chrome",
		"pbk":            "hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo",
		"sid":            "0102030405060708",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
}

func TestBuildEscapesDisplayName(t *testing.T) {
	p := base()
	p.Name = "alice smith / phone #2"
	got := Build(p)

	// A raw '#' in the fragment would truncate the label in every client.
	if strings.Count(got, "#") != 1 {
		t.Errorf("display name '#' not escaped, got: %s", got)
	}
	// '+' would be rendered literally by clients, so spaces must be %20.
	frag := got[strings.Index(got, "#")+1:]
	if strings.Contains(frag, "+") {
		t.Errorf("space encoded as '+' instead of %%20: %s", frag)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	if u.Fragment != p.Name {
		t.Errorf("fragment round trip = %q, want %q", u.Fragment, p.Name)
	}
}

func TestBuildOmitsEmptyName(t *testing.T) {
	p := base()
	p.Name = ""
	if got := Build(p); strings.Contains(got, "#") {
		t.Errorf("empty name should produce no fragment, got: %s", got)
	}
}

func TestBuildIPv6Host(t *testing.T) {
	p := base()
	p.Host = "2001:db8::1"
	got := Build(p)
	// An unbracketed IPv6 literal would make the URI unparseable.
	if !strings.Contains(got, "[2001:db8::1]:8443") {
		t.Errorf("IPv6 host not bracketed: %s", got)
	}
	if _, err := url.Parse(got); err != nil {
		t.Errorf("IPv6 URI unparseable: %v", err)
	}
}
