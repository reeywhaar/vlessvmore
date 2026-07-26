// Package stats collects per-user traffic from sing-box and enforces quotas and
// expiry.
//
// sing-box has no runtime user API, so enforcement works by omission: a user who is
// over quota or expired is marked disabled, which removes them from the next rendered
// config, which is what actually stops them connecting.
package stats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"vlessvmore/internal/store"
	"vlessvmore/internal/v2rayapi"
)

// DefaultRetention is how long hourly buckets are kept.
const DefaultRetention = 90 * 24 * time.Hour

// expiryInterval is how often expiry is re-checked independently of traffic.
// Expiry has to fire for an idle user too, so it cannot wait on a stats poll.
const expiryInterval = time.Minute

// pruneInterval is how often old buckets are dropped.
const pruneInterval = 24 * time.Hour

// Disable reasons recorded on a user when enforcement turns them off.
const (
	ReasonQuota   = "quota"
	ReasonExpired = "expired"
)

// Reloader regenerates the sing-box config. Satisfied by *singbox.Manager.
type Reloader interface {
	Reload(ctx context.Context) error
}

// Collector polls sing-box's stats service and enforces limits.
type Collector struct {
	store     *store.Store
	reload    Reloader
	log       *slog.Logger
	interval  time.Duration
	retention time.Duration

	conn   *grpc.ClientConn
	client v2rayapi.StatsServiceClient
}

// New connects to sing-box's v2ray_api endpoint.
//
// The dial is lazy, so this succeeds even when sing-box has not finished starting;
// the first poll is what surfaces a real connectivity problem.
func New(addr string, st *store.Store, reload Reloader, interval time.Duration, log *slog.Logger) (*Collector, error) {
	// Loopback, inside our own container, unauthenticated by design — TLS would
	// protect nothing here.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to sing-box stats at %s: %w", addr, err)
	}
	return &Collector{
		store:     st,
		reload:    reload,
		log:       log,
		interval:  interval,
		retention: DefaultRetention,
		conn:      conn,
		client:    v2rayapi.NewStatsServiceClient(conn),
	}, nil
}

// Close releases the gRPC connection.
func (c *Collector) Close() error { return c.conn.Close() }

// Run polls until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	collect := time.NewTicker(c.interval)
	defer collect.Stop()
	expiry := time.NewTicker(expiryInterval)
	defer expiry.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	// Enforce once at startup so a user who expired while the service was down is
	// disabled before their first reconnect rather than a minute later.
	if err := c.enforceAndReload(ctx); err != nil && ctx.Err() == nil {
		c.log.Error("initial limit enforcement failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-collect.C:
			if err := c.CollectOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				// Expected while sing-box is restarting; noisy to treat as fatal.
				c.log.Warn("collecting traffic stats failed", "error", err)
				continue
			}
			if err := c.enforceAndReload(ctx); err != nil && ctx.Err() == nil {
				c.log.Error("enforcing limits failed", "error", err)
			}

		case <-expiry.C:
			if err := c.enforceAndReload(ctx); err != nil && ctx.Err() == nil {
				c.log.Error("enforcing limits failed", "error", err)
			}

		case <-prune.C:
			cutoff := time.Now().Add(-c.retention)
			n, err := c.store.Usage.Prune(ctx, cutoff)
			if err != nil {
				c.log.Error("pruning usage history failed", "error", err)
				continue
			}
			if n > 0 {
				c.log.Info("pruned old usage buckets", "rows", n, "older_than", cutoff.UTC())
			}
		}
	}
}

// CollectOnce drains sing-box's counters into the usage store.
//
// Reset_ is true, so each poll returns traffic since the previous poll and the
// counters go back to zero. That makes every response a delta — no bookkeeping of
// previous absolute values, and no double-counting when sing-box restarts and its
// in-memory counters vanish. The cost is that traffic accrued between the last poll
// and a restart is lost; one interval of one user's traffic is an acceptable price
// for not having to reason about counter resets.
func (c *Collector) CollectOnce(ctx context.Context) error {
	resp, err := c.client.QueryStats(ctx, &v2rayapi.QueryStatsRequest{Reset_: true})
	if err != nil {
		return fmt.Errorf("query stats: %w", err)
	}

	known := map[string]bool{}
	for _, u := range c.store.Users.List() {
		known[u.ID] = true
	}

	byUser := map[string]*store.Delta{}
	var unknown []string
	for _, stat := range resp.GetStat() {
		s, ok := ParseName(stat.GetName())
		if !ok || s.Kind != KindUser {
			continue // inbound/outbound totals are not per-user; ignore them
		}
		if !known[s.ID] {
			// A user deleted since the last render can still have counters in flight.
			unknown = append(unknown, s.ID)
			continue
		}
		d := byUser[s.ID]
		if d == nil {
			d = &store.Delta{UserID: s.ID}
			byUser[s.ID] = d
		}
		switch s.Direction {
		case DirectionUp:
			d.Up += stat.GetValue()
		case DirectionDown:
			d.Down += stat.GetValue()
		}
	}
	if len(unknown) > 0 {
		c.log.Debug("discarded traffic for users that no longer exist", "ids", unknown)
	}

	deltas := make([]store.Delta, 0, len(byUser))
	for _, d := range byUser {
		deltas = append(deltas, *d)
	}
	if err := c.store.Usage.Add(ctx, time.Now(), deltas); err != nil {
		return fmt.Errorf("record usage: %w", err)
	}
	return nil
}

// enforceAndReload disables users that have run out of quota or time, and reloads
// sing-box if anything changed.
func (c *Collector) enforceAndReload(ctx context.Context) error {
	changed, err := c.Enforce(ctx)
	if err != nil {
		return err
	}
	if changed == 0 {
		return nil
	}
	if c.reload == nil {
		return nil
	}
	if err := c.reload.Reload(ctx); err != nil {
		return fmt.Errorf("reload after disabling %d user(s): %w", changed, err)
	}
	return nil
}

// Enforce disables users past their quota or expiry and reports how many changed.
func (c *Collector) Enforce(ctx context.Context) (int, error) {
	now := time.Now()
	var changed int
	var errs []error

	for _, u := range c.store.Users.List() {
		if !u.Enabled {
			continue
		}
		reason := ""
		switch {
		case u.IsExpired(now):
			reason = ReasonExpired
		default:
			over, err := c.store.OverQuota(ctx, &u)
			if err != nil {
				errs = append(errs, fmt.Errorf("check quota for %s: %w", u.Name, err))
				continue
			}
			if over {
				reason = ReasonQuota
			}
		}
		if reason == "" {
			continue
		}

		off := false
		if _, err := c.store.Users.Update(u.ID, store.Patch{Enabled: &off, DisabledReason: &reason}, now); err != nil {
			errs = append(errs, fmt.Errorf("disable %s: %w", u.Name, err))
			continue
		}
		changed++
		c.log.Info("disabled user", "name", u.Name, "id", u.ID, "reason", reason)
	}
	return changed, errors.Join(errs...)
}

// Kind is what a stat counter is attributed to.
type Kind string

// Counter kinds sing-box emits.
const (
	KindUser     Kind = "user"
	KindInbound  Kind = "inbound"
	KindOutbound Kind = "outbound"
)

// Direction is which way the traffic went, from the server's point of view.
type Direction string

// Traffic directions sing-box emits.
const (
	DirectionUp   Direction = "uplink"
	DirectionDown Direction = "downlink"
)

// separator is what sing-box joins stat name components with.
const separator = ">>>"

// Stat is a parsed counter name.
type Stat struct {
	Kind Kind
	// ID is the user's internal id or the inbound/outbound tag.
	ID        string
	Direction Direction
}

// ParseName splits a sing-box stat counter name.
//
// The format is `user>>><id>>>>traffic>>>uplink` — components joined by ">>>". Note
// that an id may itself contain no separator, which our ids never do.
func ParseName(name string) (Stat, bool) {
	parts := strings.Split(name, separator)
	if len(parts) != 4 || parts[2] != "traffic" {
		return Stat{}, false
	}
	kind := Kind(parts[0])
	switch kind {
	case KindUser, KindInbound, KindOutbound:
	default:
		return Stat{}, false
	}
	dir := Direction(parts[3])
	switch dir {
	case DirectionUp, DirectionDown:
	default:
		return Stat{}, false
	}
	if parts[1] == "" {
		return Stat{}, false
	}
	return Stat{Kind: kind, ID: parts[1], Direction: dir}, true
}
