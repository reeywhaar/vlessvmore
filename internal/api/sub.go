package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"vlessvmore/internal/link"
	"vlessvmore/internal/store"
)

// SubPath is the prefix clients poll. Deliberately outside /api: it is the one route
// that must work without an Authorization header, because subscription clients cannot
// send one.
const SubPath = "/sub/"

// subUpdateHours is advertised to clients as how often to refresh. Daily is frequent
// enough that a revoked user notices, and rare enough to be invisible in the logs.
const subUpdateHours = 24

// subscription serves a user's credential to their client.
//
// Authentication is the URL itself: the token in the path is a 160-bit capability. That
// is the standard arrangement for subscription URLs and it has to be, since no client
// will attach a bearer token — but it does mean the URL is exactly as sensitive as the
// credential it returns.
//
// An unknown token gets a plain 404, identical to any other unmatched path, so probing
// this endpoint reveals nothing about whether it is a subscription server at all.
func (s *Server) subscription(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	u, err := s.store.Users.GetBySubToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	uri, err := link.ForUser(s.cfg, s.store.Identity, u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Sent even for a disabled or expired user, along with an honest
	// Subscription-Userinfo header. The credential is already theirs, and it will not
	// work — they are absent from sing-box's config — but clients render the header as
	// "quota exhausted" or "expired", which is a far better answer than a bare error.
	if err := s.setSubHeaders(r, w, u); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	format := r.URL.Query().Get("format")
	switch format {
	case "", "base64":
		// The de-facto default: base64 of newline-separated URIs. Standard encoding
		// with padding, which is what clients expect here — unlike the raw URL-safe
		// encoding used for key material elsewhere in this codebase.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(uri))))
	case "uri", "plain":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(uri + "\n"))
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("format %q must be base64 or uri", format))
	}
}

// setSubHeaders adds the headers subscription clients read.
//
// Subscription-Userinfo is what makes a client show remaining traffic and an expiry
// date in its own UI, which is the whole reason to prefer a subscription URL over
// handing someone a raw link. It is a de-facto standard rather than a specified one:
// bytes for upload/download/total, unix seconds for expire, and zero meaning unlimited
// or never.
func (s *Server) setSubHeaders(r *http.Request, w http.ResponseWriter, u *store.User) error {
	up, down, err := s.store.Usage.TotalSince(r.Context(), u.ID, u.UsageResetAt)
	if err != nil {
		return err
	}

	// Fields are omitted rather than sent as zero when they do not apply.
	//
	// "total=0 means unlimited" is the usual convention, but clients do not reliably
	// implement it: Hiddify shown total=0 invents a ceiling of its own (~85.9 GiB, the
	// leading digits of MaxInt64) and reports a limit the server never set. Leaving the
	// field out gives it nothing to misread. Same reasoning for expire.
	fields := []string{
		fmt.Sprintf("upload=%d", up),
		fmt.Sprintf("download=%d", down),
	}
	if u.QuotaBytes > 0 {
		fields = append(fields, fmt.Sprintf("total=%d", u.QuotaBytes))
	}
	if u.ExpiresAt != nil {
		fields = append(fields, fmt.Sprintf("expire=%d", u.ExpiresAt.Unix()))
	}
	w.Header().Set("Subscription-Userinfo", strings.Join(fields, "; "))

	w.Header().Set("Profile-Update-Interval", fmt.Sprint(subUpdateHours))
	// The configured server name when set, else the user's own — matching the vless://
	// fragment, so a client shows the same label however the profile was added.
	label := s.cfg.ClientLabel(u.Name)
	// Base64 because the title may contain non-ASCII, which a raw header cannot carry.
	w.Header().Set("Profile-Title", "base64:"+base64.StdEncoding.EncodeToString([]byte(label)))

	// Gives the client a sensible filename if the user saves the response instead of
	// subscribing to it.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%q", sanitizeFilename(label)+".txt"))

	// Never cache a credential: a revoked or re-issued subscription has to take effect
	// on the next poll, not whenever an intermediary decides to expire it.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	return nil
}

// sanitizeFilename keeps a display name usable in a Content-Disposition filename.
//
// Only alphanumerics, dashes and underscores survive. Dots are dropped too: the filename
// is a suggestion, so there is nothing to gain from allowing ".." through and no reason
// to make a client's path handling part of our threat model.
func sanitizeFilename(name string) string {
	replaced := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	if replaced == "" {
		return "subscription"
	}
	return replaced
}

// SubscriptionURL is the URL to hand to a client for this user.
func (s *Server) SubscriptionURL(u *store.User) string {
	return SubscriptionURL(s.cfg.SubscriptionBase(), u)
}

// SubscriptionURL builds a user's subscription URL on the given origin.
func SubscriptionURL(base string, u *store.User) string {
	if u.SubToken == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + SubPath + u.SubToken
}
