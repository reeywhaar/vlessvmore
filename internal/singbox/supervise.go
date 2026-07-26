package singbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Backoff bounds for restarting a sing-box that exited unexpectedly. Starting small
// keeps a transient failure invisible; capping it keeps a persistent one from
// hammering the host while still recovering unattended.
const (
	minBackoff = 500 * time.Millisecond
	maxBackoff = 30 * time.Second
	// resetBackoffAfter is how long a process must stay up before its next crash is
	// treated as a fresh problem rather than a continuing one.
	resetBackoffAfter = time.Minute
)

// Supervisor runs sing-box as a child process and keeps it running.
type Supervisor struct {
	configPath string
	log        *slog.Logger

	mu      sync.Mutex
	cmd     *exec.Cmd
	started time.Time
	exited  bool

	done chan struct{}
	wg   sync.WaitGroup
}

// NewSupervisor prepares a supervisor for the config at path.
func NewSupervisor(configPath string, log *slog.Logger) *Supervisor {
	return &Supervisor{
		configPath: configPath,
		log:        log,
		done:       make(chan struct{}),
	}
}

// Start launches sing-box and supervises it until ctx is cancelled.
//
// The first launch is synchronous so a config that sing-box refuses outright is
// reported to the operator immediately rather than disappearing into a restart loop.
func (s *Supervisor) Start(ctx context.Context) error {
	if err := s.spawn(); err != nil {
		return err
	}
	s.wg.Add(1)
	go s.supervise(ctx)
	return nil
}

// spawn starts one sing-box process.
func (s *Supervisor) spawn() error {
	cmd := exec.Command(Binary(), "run", "-c", s.configPath)
	// Inherit stdio so `docker logs` shows sing-box's own output unchanged. Operators
	// already know how to read those logs; wrapping them would only obscure things.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Its own process group, so a Ctrl-C delivered to the whole group does not race
	// our own orderly shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", Binary(), err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.started = time.Now()
	s.exited = false
	s.mu.Unlock()

	s.log.Info("sing-box started", "pid", cmd.Process.Pid, "config", s.configPath)
	return nil
}

// supervise waits on the child and restarts it if it exits before ctx is done.
func (s *Supervisor) supervise(ctx context.Context) {
	defer s.wg.Done()
	defer close(s.done)

	backoff := minBackoff
	for {
		s.mu.Lock()
		cmd := s.cmd
		started := s.started
		s.mu.Unlock()

		// cmd is nil when the previous respawn failed; there is nothing to wait for,
		// so fall through to the backoff and try again.
		if cmd != nil {
			err := cmd.Wait()

			s.mu.Lock()
			s.exited = true
			s.mu.Unlock()

			if ctx.Err() != nil {
				// Expected: we are shutting down and asked it to stop.
				s.log.Info("sing-box exited during shutdown", "error", err)
				return
			}
			// A process that stayed up a while then died is a new problem, not a
			// continuing one, so it does not inherit the accumulated backoff.
			if time.Since(started) > resetBackoffAfter {
				backoff = minBackoff
			}
			s.log.Error("sing-box exited unexpectedly; restarting",
				"error", err, "uptime", time.Since(started).Truncate(time.Millisecond), "backoff", backoff)
		}

		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)

		if err := s.spawn(); err != nil {
			// The binary itself is unusable: missing, not executable, bad mount.
			// Keep retrying on the same schedule rather than giving up — the operator
			// may be fixing it right now, and exiting would take the API down too.
			s.log.Error("could not restart sing-box", "error", err)
			s.mu.Lock()
			s.cmd = nil
			s.exited = true
			s.mu.Unlock()
		}
	}
}

// Reload asks the running sing-box to re-read its config.
//
// sing-box validates on SIGHUP and keeps the old instance if the new config is bad,
// which combined with our pre-flight Check makes a reload effectively safe. What it
// is not is transparent: recreating the instance drops every established TCP
// connection, so clients reconnect. That is inherent to sing-box having no runtime
// user API, and is why reloads are coalesced.
func (s *Supervisor) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd == nil || s.cmd.Process == nil {
		return errors.New("sing-box is not running")
	}
	if s.exited {
		// The supervisor will restart it with the current config, which already
		// includes whatever change prompted this reload.
		return nil
	}
	if err := s.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal sing-box: %w", err)
	}
	return nil
}

// PID returns the running child's pid, or 0.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil || s.exited {
		return 0
	}
	return s.cmd.Process.Pid
}

// Running reports whether sing-box is up.
func (s *Supervisor) Running() bool { return s.PID() != 0 }

// StartedAt is when the current process launched.
func (s *Supervisor) StartedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Stop asks sing-box to terminate and waits for it, escalating to SIGKILL after
// grace. Call it after cancelling the context passed to Start.
func (s *Supervisor) Stop(grace time.Duration) error {
	s.mu.Lock()
	cmd := s.cmd
	exited := s.exited
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil || exited {
		return nil
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("terminate sing-box: %w", err)
	}

	select {
	case <-s.done:
	case <-time.After(grace):
		s.log.Warn("sing-box did not exit in time; killing", "grace", grace)
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill sing-box: %w", err)
		}
		<-s.done
	}
	s.wg.Wait()
	return nil
}
