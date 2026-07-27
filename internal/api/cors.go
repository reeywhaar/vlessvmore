package api

import (
	"net/http"
	"strings"
)

// A preflight carries no Authorization header — browsers do not send one, ever. So any
// OPTIONS this server answers with a 204 is answered to an unauthenticated stranger, and
// answering only for real paths tells that stranger which paths are real. That is exactly
// the discrimination notFound exists to prevent, so the gate is the Origin: an origin that
// is not on the list gets the same padded 404 as everything else, and only a caller who
// already knows the dashboard's hostname learns anything.
//
// Configuring "*" gives that up deliberately. It is the difference between "someone who
// knows where the dashboard lives can enumerate /api" and "anyone can", and it is the
// operator's call to make.
type corsPolicy struct {
	any     bool
	origins []string
}

// newCORSPolicy builds the policy from config.json's cors_origins. A nil result disables
// CORS entirely, and the middleware is then not installed at all.
//
// Entries arrive already lowercased and stripped of a trailing slash by the config's
// applyDefaults, so matching here is a plain string compare.
func newCORSPolicy(origins []string) *corsPolicy {
	p := &corsPolicy{}
	for _, o := range origins {
		if o == "*" {
			p.any = true
			continue
		}
		if o != "" {
			p.origins = append(p.origins, o)
		}
	}
	if !p.any && len(p.origins) == 0 {
		return nil
	}
	return p
}

// allow returns the value to echo in Access-Control-Allow-Origin, or "" to refuse.
func (p *corsPolicy) allow(origin string) string {
	if p == nil || origin == "" {
		return ""
	}
	if p.any {
		// "*" rather than the origin itself: cacheable, and it makes a single response
		// enough to tell that this deployment is wide open.
		return "*"
	}
	origin = strings.ToLower(strings.TrimRight(origin, "/"))
	for _, o := range p.origins {
		if o == origin {
			// Echoed, because naming one host is the only way to allow just that host.
			return origin
		}
	}
	return ""
}

// corsMethods and corsHeaders are what the API actually uses. Listed rather than
// reflected back from the request, so a preflight advertises nothing we do not serve.
const (
	corsMethods = "GET, POST, PATCH, DELETE"
	corsHeaders = "Authorization, Content-Type"
	corsMaxAge  = "600"
)

// cors answers preflights for allowed origins and tags real responses. A request with no
// Origin, or an Origin that is not allowed, passes through untouched — so curl and the
// CLI behave exactly as they did before, and a refused preflight lands on the catch-all.
func (s *Server) cors(p *corsPolicy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := p.allow(r.Header.Get("Origin"))
		if allowed == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Vary even when echoing "*": a shared cache that stored one origin's response
		// must not serve it to another after the policy changes.
		w.Header().Add("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Origin", allowed)

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", corsMethods)
			w.Header().Set("Access-Control-Allow-Headers", corsHeaders)
			w.Header().Set("Access-Control-Max-Age", corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
