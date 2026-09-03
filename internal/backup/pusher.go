package backup

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"vlessvmore/internal/config"
)

// pushTimeout bounds one pass, end to end.
//
// Generous, because it covers building the archive as well as sending it: `VACUUM INTO` over
// a long traffic history takes a moment, and the upload leaves this machine. A backup cut
// short would fail exactly on the deployments that most need one.
const pushTimeout = 10 * time.Minute

// Pusher copies the deployment to a backup agent, on the terms the mode sets.
type Pusher struct {
	Source

	// URL is where an agent takes an archive. Empty means nothing is sent at all.
	URL  string
	Mode config.BackupMode

	// Every is how often to look, and defaults to [config.BackupDelay].
	//
	// A field only so a test can drive the loop without waiting five minutes for it.
	// Nothing reads it from config.json: see the constants in that package for why neither
	// timing here is a setting.
	Every time.Duration

	Client *http.Client
}

// Run works until the context is done.
//
// The first pass is immediate rather than one interval in. A process that has just started is
// the one most likely to have been restarted onto a new volume, or to have just had its
// keypair regenerated, and waiting five minutes to find that out is five minutes in a state
// nobody has a copy of.
//
// The interval is also the throttle, and that is most of what it is for. An operator adding
// six users writes to users.json six times in a minute; looking on a timer rather than on
// every write turns that into one archive, five minutes later, holding all six. Nothing here
// reacts to a write directly, so there is no burst it can be made to keep up with.
func (p *Pusher) Run(ctx context.Context) {
	if p.URL == "" {
		return
	}
	if p.Every <= 0 {
		p.Every = config.BackupDelay
	}
	p.Log.Info("backing up", "to", p.URL, "mode", p.Mode, "after_a_change_within", p.Every,
		"carries_stats", p.Mode.Stats(), "at_least_every", p.Mode.Period())

	for {
		if err := p.Once(ctx); err != nil && ctx.Err() == nil {
			// Logged and left for the next pass rather than retried here. What fails is
			// either transient — the agent restarting, the remote unreachable — or needs
			// a person, and neither is helped by trying again a second later.
			p.Log.Error("backup failed", "error", err)
		}

		timer := time.NewTimer(p.Every)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// client is the one this was given, or one of our own.
//
// Defaulted here rather than in [Pusher.Run], because Once is reachable without it — a test
// drives a single pass, and a caller that did the same against a nil client got a panic
// rather than a backup.
func (p *Pusher) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	p.Client = &http.Client{Timeout: pushTimeout}
	return p.Client
}

// Once builds a copy, sends it if the mode says it is due, and remembers what was sent.
//
// Exported so a test can drive one pass without waiting on a clock.
func (p *Pusher) Once(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	now := time.Now()
	last, at, err := p.Store.Usage.LastBackup(ctx)
	if err != nil {
		return err
	}

	// The cheap half first, and only to be hashed. There is nothing cheaper that answers
	// the question: an mtime moves for a write that changed nothing, and a size is shared
	// by files that differ. Probing without stats.db means a deployment carrying a year of
	// traffic history is not vacuuming it every five minutes to find out whether anybody
	// touched a user.
	probe, err := p.Probe(ctx)
	if err != nil {
		return err
	}
	changed := !bytes.Equal(last, probe)

	// And the floor, which only "all" has. It is what catches the one thing hashing the
	// JSON files never sees: traffic. A server nobody administers for a week still meters
	// every byte through it, and to the check above that week looks exactly like an idle
	// one.
	//
	// A push clears both at once. The digest it records is the one the archive was built
	// from, so a change that was waiting for its delay has gone out with it — there is
	// nothing left pending, and the floor starts again from now.
	period := p.Mode.Period()
	due := period > 0 && !at.IsZero() && now.Sub(at) >= period

	if !changed && !due {
		p.Log.Debug("nothing to back up; the deployment is as it was",
			"since", at.Format(time.RFC3339), "floor", period)
		return nil
	}

	archive, err := p.Build(ctx, p.Mode.Stats(), now)
	if err != nil {
		return err
	}

	name := Filename(now)
	if err := Push(ctx, p.client(), p.URL, name, archive.Body); err != nil {
		return err
	}

	// Only now. Recorded before the agent accepted it, a rejected upload would leave this
	// process believing a copy exists that does not — and on a deployment nobody is
	// editing, the next change to a user would be the only thing that ever made it try
	// again.
	if err := p.Store.Usage.RecordBackup(ctx, archive.Digest, now); err != nil {
		return err
	}

	p.Log.Info("backed up", "name", name, "bytes", len(archive.Body),
		"mode", p.Mode, "changed", changed, "first", at.IsZero())
	return nil
}
