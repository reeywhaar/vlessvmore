package api

import (
	"context"
	"crypto/rand"
	"math/big"
	"net/http"
	"time"
)

// Every refusal goes through notFound and comes out identical, in content and in timing.
// Nobody gets to map the bun stash one 401 or one fast 404 at a time.

// Variables rather than constants so tests can shrink them; nothing else reassigns these.
var (
	antibunstealFloor  = 60 * time.Millisecond
	antibunstealJitter = 90 * time.Millisecond
)

type antibunstealKey struct{}

type antibunstealState struct {
	start time.Time
	pad   bool
}

// antibunsteal records the arrival time padUntil measures from, before any handler work.
func (s *Server) antibunsteal(public bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := antibunstealState{start: time.Now(), pad: public}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), antibunstealKey{}, st)))
	})
}

// notFound refuses a request. The body is stdlib http.NotFound's, byte for byte.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if st, ok := r.Context().Value(antibunstealKey{}).(antibunstealState); ok && st.pad {
		padUntil(r.Context(), st.start)
	}
	http.NotFound(w, r)
}

// padUntil sleeps to an absolute deadline, which makes the total time independent of how
// long the work took. A random delay *added* to the work would only be noise, and enough
// samples average it away.
func padUntil(ctx context.Context, start time.Time) {
	d := time.Until(start.Add(antibunstealFloor + jitter(antibunstealJitter)))
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// jitter returns a uniform duration in [0, max). crypto/rand because a predictable
// sequence could be subtracted back out.
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}
