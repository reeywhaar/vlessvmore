// Package api serves the management HTTP JSON API.
//
// The same handler is served on two listeners with different trust: a TCP port that
// requires a bearer token, and a unix socket inside the container that does not.
// Reaching the socket already requires being root in the container, which is strictly
// more access than any token grants, so a second check there would be theatre — and
// it is what lets the CLI work before any token exists.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vlessvmore/internal/config"
	"vlessvmore/internal/link"
	"vlessvmore/internal/singbox"
	"vlessvmore/internal/store"
)

// Server holds everything the handlers need.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	manager *singbox.Manager
	log     *slog.Logger
}

// New builds a server.
func New(cfg *config.Config, st *store.Store, mgr *singbox.Manager, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: st, manager: mgr, log: log}
}

// Handler returns the routes. When requireAuth is false every request is treated as
// trusted, which is only correct for the unix socket.
func (s *Server) Handler(requireAuth bool) http.Handler {
	mux := http.NewServeMux()

	// Takes unmatched paths, and also method mismatches, which ServeMux would otherwise
	// answer with a 405 that admits the path exists.
	mux.HandleFunc("/", s.notFound)

	// Socket only. A JSON health check answering strangers is a fingerprint no static
	// site has, and the image's HEALTHCHECK runs `vlessvmore status`, which comes in here.
	if !requireAuth {
		mux.HandleFunc("GET /healthz", s.healthz)
	}

	// The cover page. When a reverse proxy fronts this service on the same hostname
	// Reality uses for its handshake, that hostname has to look like an ordinary web
	// server to anyone who visits it. `{$}` matches only the exact root, so unknown
	// paths still fall through to a 404 instead of this returning 200 for everything.
	mux.HandleFunc("GET /{$}", s.root)

	// Subscription URLs. Unauthenticated by necessity — the token in the path is the
	// credential — and outside /api so it is obvious this route is public.
	mux.HandleFunc("GET "+SubPath+"{token}", s.subscription)

	// The install page and its assets, on the same credential.
	mux.HandleFunc("GET "+ShowPath+"{token}", s.show)
	mux.HandleFunc("GET "+StaticPath+"{name}", s.staticAsset)

	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/reload", s.reload)

	mux.HandleFunc("GET /api/users", s.listUsers)
	mux.HandleFunc("POST /api/users", s.createUser)
	mux.HandleFunc("GET /api/users/{id}", s.getUser)
	mux.HandleFunc("PATCH /api/users/{id}", s.patchUser)
	mux.HandleFunc("DELETE /api/users/{id}", s.deleteUser)
	mux.HandleFunc("POST /api/users/{id}/reset-usage", s.resetUsage)
	mux.HandleFunc("GET /api/users/{id}/usage", s.userUsage)
	mux.HandleFunc("GET /api/users/{id}/link", s.userLink)
	mux.HandleFunc("POST /api/users/{id}/rotate-sub", s.rotateSubToken)

	mux.HandleFunc("GET /api/server", s.serverInfo)

	mux.HandleFunc("GET /api/tokens", s.listTokens)
	mux.HandleFunc("POST /api/tokens", s.createToken)
	mux.HandleFunc("DELETE /api/tokens/{id}", s.deleteToken)

	var h http.Handler = mux
	if requireAuth {
		h = s.authenticate(h)
	}
	// Outside authenticate, because a preflight has no credential to check and must be
	// answered — or refused — before the bearer check ever looks at it.
	if p := newCORSPolicy(s.cfg.CORSOrigins); p != nil {
		h = s.cors(p, h)
	}
	return s.logRequests(s.antibunsteal(requireAuth, h))
}

// isPublic reports whether a path is reachable without a bearer token. Prefixes, not
// exact paths — so nothing private may ever be registered under one.
func isPublic(path string) bool {
	if path == "/" {
		return true
	}
	for _, prefix := range []string{SubPath, ShowPath, StaticPath} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// authenticate enforces a bearer token on everything that is not public. A rejection is
// notFound, not a 401: a legitimate caller already knows the endpoint is there, and
// WWW-Authenticate would name the software in its realm.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		secret, ok := bearer(r)
		if !ok {
			s.notFound(w, r)
			return
		}
		if !s.validSecret(secret, time.Now()) {
			s.notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validSecret accepts any token minted by `vlessvmore token create`.
//
// Stored tokens are the only credential: there is no bootstrap secret to configure or
// forget to remove. Automation gets its token the same way a person does — over the
// container's unix socket, which needs no token itself.
func (s *Server) validSecret(secret string, now time.Time) bool {
	_, err := s.store.Tokens.Lookup(secret, now)
	return err == nil
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

// logRequests records failures. Successful reads are not interesting enough to log
// on every poll from a dashboard.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status >= 400 {
			s.log.Warn("api request failed",
				"method", r.Method, "path", r.URL.Path, "status", rec.status)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ---- handlers ----

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// root answers the bare hostname with a plain 200, so a proxied host looks like an
// unremarkable web server. It reveals nothing about what actually runs here.
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// StatusResponse describes the running service.
type StatusResponse struct {
	SingBox      singbox.Status `json:"sing_box"`
	SingBoxBuild string         `json:"sing_box_version,omitempty"`
	Users        int            `json:"users"`
	ActiveUsers  int            `json:"active_users"`
	Tokens       int            `json:"tokens"`
	DataDir      string         `json:"data_dir"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	active, err := s.store.ActiveUsers(r.Context(), time.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resp := StatusResponse{
		SingBox:     s.manager.Status(),
		Users:       len(s.store.Users.List()),
		ActiveUsers: len(active),
		Tokens:      len(s.store.Tokens.List()),
		DataDir:     s.store.Dir(),
	}
	// Best effort: the version shells out, and a status endpoint should not fail
	// because that did.
	if v, err := singbox.Version(r.Context()); err == nil {
		resp.SingBoxBuild = v
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.Reload(r.Context()); err != nil {
		// A rejected config is the caller's problem to see: the running proxy is
		// untouched, but their intended change is not live.
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.manager.Status())
}

// UserResponse is a user plus derived fields: usage, and the subscription URL to hand
// to their client.
type UserResponse struct {
	store.User
	Usage           *UsageSummary `json:"usage,omitempty"`
	SubscriptionURL string        `json:"subscription_url,omitempty"`
	InstallURL      string        `json:"install_url,omitempty"`
}

// userResponse derives both URLs, so no handler has to remember to.
func (s *Server) userResponse(u *store.User) UserResponse {
	return UserResponse{
		User:            *u,
		SubscriptionURL: s.SubscriptionURL(u),
		InstallURL:      s.ShowURL(u),
	}
}

// UsageSummary is a user's traffic, both lifetime and within the current quota window.
type UsageSummary struct {
	Up             int64 `json:"up"`
	Down           int64 `json:"down"`
	Total          int64 `json:"total"`
	WindowUp       int64 `json:"window_up"`
	WindowDown     int64 `json:"window_down"`
	WindowTotal    int64 `json:"window_total"`
	QuotaBytes     int64 `json:"quota_bytes"`
	QuotaRemaining int64 `json:"quota_remaining"`
}

func (s *Server) usageSummary(r *http.Request, u *store.User) (*UsageSummary, error) {
	up, down, err := s.store.Usage.Total(r.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	wUp, wDown, err := s.store.Usage.TotalSince(r.Context(), u.ID, u.UsageResetAt)
	if err != nil {
		return nil, err
	}
	sum := &UsageSummary{
		Up: up, Down: down, Total: up + down,
		WindowUp: wUp, WindowDown: wDown, WindowTotal: wUp + wDown,
		QuotaBytes: u.QuotaBytes,
	}
	if u.QuotaBytes > 0 {
		sum.QuotaRemaining = max(u.QuotaBytes-sum.WindowTotal, 0)
	}
	return sum, nil
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users := s.store.Users.List()
	withUsage := r.URL.Query().Get("include") == "usage"

	out := make([]UserResponse, 0, len(users))
	for i := range users {
		resp := s.userResponse(&users[i])
		if withUsage {
			sum, err := s.usageSummary(r, &users[i])
			if err != nil {
				writeStoreError(w, err)
				return
			}
			resp.Usage = sum
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.Users.Get(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resp := s.userResponse(u)
	sum, err := s.usageSummary(r, u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resp.Usage = sum
	writeJSON(w, http.StatusOK, resp)
}

// CreateUserRequest is the body of POST /api/users.
type CreateUserRequest struct {
	Name       string     `json:"name"`
	UUID       string     `json:"uuid,omitempty"`
	QuotaBytes int64      `json:"quota_bytes,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Enabled    *bool      `json:"enabled,omitempty"`
	Note       string     `json:"note,omitempty"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if !decode(w, r, &req) {
		return
	}
	p := store.CreateParams{
		Name:       req.Name,
		UUID:       req.UUID,
		QuotaBytes: req.QuotaBytes,
		ExpiresAt:  req.ExpiresAt,
		Note:       req.Note,
	}
	if req.Enabled != nil {
		p.Disabled = !*req.Enabled
	}
	u, err := s.store.Users.Create(p, time.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.reloadAfterChange(r, w, http.StatusCreated, s.userResponse(u))
}

// PatchUserRequest is the body of PATCH /api/users/{id}. Absent fields are unchanged;
// a present null clears expires_at.
type PatchUserRequest struct {
	Name       *string    `json:"name,omitempty"`
	UUID       *string    `json:"uuid,omitempty"`
	Enabled    *bool      `json:"enabled,omitempty"`
	QuotaBytes *int64     `json:"quota_bytes,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Note       *string    `json:"note,omitempty"`
}

func (s *Server) patchUser(w http.ResponseWriter, r *http.Request) {
	// Decoded twice: once into the typed struct, once into a map, because JSON cannot
	// otherwise distinguish `"expires_at": null` (clear it) from an absent key (leave
	// it alone) — and clearing an expiry is something an operator needs to do.
	var raw map[string]json.RawMessage
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	var req PatchUserRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	patch := store.Patch{
		Name:       req.Name,
		UUID:       req.UUID,
		Enabled:    req.Enabled,
		QuotaBytes: req.QuotaBytes,
		Note:       req.Note,
	}
	if _, present := raw["expires_at"]; present {
		exp := req.ExpiresAt // nil when the caller sent null
		patch.ExpiresAt = &exp
	}

	u, err := s.store.Users.Update(r.PathValue("id"), patch, time.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.reloadAfterChange(r, w, http.StatusOK, s.userResponse(u))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.Users.Get(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.store.DeleteUser(r.Context(), u.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	s.reloadAfterChange(r, w, http.StatusOK, map[string]any{"deleted": u.ID, "name": u.Name})
}

func (s *Server) resetUsage(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.Users.ResetUsageWindow(r.PathValue("id"), time.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Resetting can re-enable a quota-disabled user, so the config may change.
	s.reloadAfterChange(r, w, http.StatusOK, s.userResponse(u))
}

func (s *Server) userUsage(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.Users.Get(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	q := r.URL.Query()

	width := time.Hour
	switch b := q.Get("bucket"); b {
	case "", "hour":
	case "day":
		width = 24 * time.Hour
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("bucket %q must be hour or day", b))
		return
	}

	now := time.Now()
	to := now
	from := now.Add(-7 * 24 * time.Hour)
	if v := q.Get("from"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "from: "+err.Error())
			return
		}
		from = t
	}
	if v := q.Get("to"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "to: "+err.Error())
			return
		}
		to = t
	}
	if to.Before(from) {
		writeError(w, http.StatusBadRequest, "to must not be before from")
		return
	}

	series, err := s.store.Usage.Series(r.Context(), u.ID, from, to, width)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	summary, err := s.usageSummary(r, u)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": u.ID,
		"name":    u.Name,
		"from":    from.UTC(),
		"to":      to.UTC(),
		"bucket":  map[bool]string{true: "day", false: "hour"}[width > time.Hour],
		"series":  series,
		"summary": summary,
	})
}

func (s *Server) userLink(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.Users.Get(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	uri, err := link.ForUser(s.cfg, s.store.Identity, u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := LinkResponse{
		UserID:          u.ID,
		Name:            u.Name,
		Link:            uri,
		SubscriptionURL: s.SubscriptionURL(u),
		InstallURL:      s.ShowURL(u),
	}

	// The QR matrices are included by default: a caller asking for a link is almost
	// always about to show it to someone with a phone. `?qr=false` opts out for
	// callers that only want the URIs.
	if r.URL.Query().Get("qr") != "false" {
		code, err := link.Encode(uri)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp.QR = code

		// The subscription gets its own, and it is the one to prefer: a scanned
		// subscription re-fetches, so it survives a key rotation or a changed port,
		// while a scanned link is frozen at the moment it was drawn. Absent only for
		// a user with no subscription token.
		if resp.SubscriptionURL != "" {
			subCode, err := link.Encode(resp.SubscriptionURL)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			resp.SubscriptionQR = subCode
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// LinkResponse is a user's connection URI and subscription URL, each also as a QR matrix.
type LinkResponse struct {
	UserID          string `json:"user_id"`
	Name            string `json:"name"`
	Link            string `json:"link"`
	SubscriptionURL string `json:"subscription_url,omitempty"`
	InstallURL      string `json:"install_url,omitempty"`

	// QR encodes Link, SubscriptionQR encodes SubscriptionURL. Two fields rather than
	// one switched by a query parameter: a caller drawing both — "scan this to connect
	// now, or this to subscribe" — should not have to ask twice.
	QR             *link.QR `json:"qr,omitempty"`
	SubscriptionQR *link.QR `json:"subscription_qr,omitempty"`
}

// rotateSubToken issues a new subscription URL, invalidating the old one. The user's
// UUID is untouched, so an already-configured client keeps working — this cuts off a
// leaked subscription link without disconnecting anyone.
func (s *Server) rotateSubToken(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.Users.RotateSubToken(r.PathValue("id"), time.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// No reload: the credential did not change, so sing-box's config is unaffected.
	writeJSON(w, http.StatusOK, s.userResponse(u))
}

// ServerResponse is the connection information a client needs, minus the secret half.
type ServerResponse struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	SNI         string `json:"sni"`
	PublicKey   string `json:"public_key"`
	ShortID     string `json:"short_id"`
	Flow        string `json:"flow"`
	Fingerprint string `json:"fingerprint"`
	Handshake   string `json:"handshake"`
}

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	pub, err := s.store.Identity.PublicKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Only the derived public key is ever returned; the private key stays in
	// config.json and never crosses this boundary.
	writeJSON(w, http.StatusOK, ServerResponse{
		Host:        s.cfg.Host,
		Port:        s.cfg.Port,
		SNI:         s.cfg.SNI,
		PublicKey:   pub,
		ShortID:     s.store.Identity.Get().ShortID,
		Flow:        s.cfg.FlowValue(),
		Fingerprint: s.cfg.Fingerprint,
		Handshake:   fmt.Sprintf("%s:%d", s.cfg.Handshake.Server, s.cfg.Handshake.ServerPort),
	})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tokens": s.store.Tokens.List()})
}

// CreateTokenRequest is the body of POST /api/tokens.
type CreateTokenRequest struct {
	Label string `json:"label"`
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req CreateTokenRequest
	if !decode(w, r, &req) {
		return
	}
	tok, secret, err := s.store.Tokens.Create(req.Label, time.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The secret appears here and nowhere else, ever again.
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "secret": secret})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	if err := s.store.Tokens.Delete(ref); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": ref})
}

// reloadAfterChange regenerates the config and reports the result alongside the
// change that was already persisted.
//
// The write has happened either way, so a reload failure is not a 500: the caller's
// change is recorded and will take effect on the next successful reload. Reporting it
// in the body rather than the status keeps that distinction visible.
func (s *Server) reloadAfterChange(r *http.Request, w http.ResponseWriter, code int, payload any) {
	err := s.manager.Reload(r.Context())
	body := map[string]any{"result": payload, "reloaded": err == nil}
	if err != nil {
		body["reload_error"] = err.Error()
		s.log.Error("reload after change failed", "error", err)
	}
	writeJSON(w, code, body)
}

// ---- helpers ----

func parseTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	// A bare unix timestamp is convenient for dashboards.
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is not an RFC3339 timestamp, a YYYY-MM-DD date, or a unix time", v)
}

// maxBody caps request bodies. Every body here is a small JSON object; the limit
// exists so a malformed or hostile request cannot make us allocate without bound.
const maxBody = 1 << 20

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	defer r.Body.Close()
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading body: "+err.Error())
		return nil, false
	}
	return b, true
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "a JSON body is required")
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// writeStoreError maps store failures onto HTTP status codes so clients can react
// programmatically instead of matching on message text.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		// Anything unclassified is ours, not the caller's: a failed disk write or a
		// database error must not be reported as bad input.
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
