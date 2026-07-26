// Package link builds the vless:// URIs clients import.
package link

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Params is everything a vless/Reality URI needs. PublicKey is the derived Reality
// public key, never the private one.
type Params struct {
	UUID        string
	Host        string
	Port        int
	SNI         string
	PublicKey   string
	ShortID     string
	Flow        string // omitted from the URI when empty
	Fingerprint string
	Name        string // becomes the #fragment, i.e. the label the client shows
}

// Build renders the URI.
//
// Query parameters are emitted in a fixed order rather than via url.Values.Encode,
// which sorts alphabetically. Order is functionally irrelevant to clients, but a
// stable order keeps this byte-identical to the format the previous deployment
// documented, so existing notes and golden tests stay valid.
func Build(p Params) string {
	q := make([]string, 0, 9)
	add := func(k, v string) {
		q = append(q, k+"="+url.QueryEscape(v))
	}

	add("type", "tcp")
	add("encryption", "none")
	if p.Flow != "" {
		add("flow", p.Flow)
	}
	add("packetEncoding", "xudp")
	add("security", "reality")
	add("sni", p.SNI)
	if p.Fingerprint != "" {
		add("fp", p.Fingerprint)
	}
	add("pbk", p.PublicKey)
	add("sid", p.ShortID)

	var b strings.Builder
	b.WriteString("vless://")
	b.WriteString(url.User(p.UUID).String())
	b.WriteByte('@')
	b.WriteString(net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
	b.WriteByte('?')
	b.WriteString(strings.Join(q, "&"))
	if p.Name != "" {
		b.WriteByte('#')
		// PathEscape rather than QueryEscape: a space in a display name should show
		// up as %20, not '+', which clients would render literally.
		b.WriteString(url.PathEscape(p.Name))
	}
	return b.String()
}
