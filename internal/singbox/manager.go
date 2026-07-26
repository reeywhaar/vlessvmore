package singbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"vlessvmore/internal/config"
	"vlessvmore/internal/store"
)

// DefaultDebounce is how long a reload request waits for company.
//
// Every reload drops established connections, so a script adding ten users should
// cause one interruption rather than ten. A second is short enough that an operator
// running a single command does not notice it.
const DefaultDebounce = time.Second

// Manager owns the rendered config and the sing-box process.
type Manager struct {
	cfg      *config.Config
	store    *store.Store
	renderer *Renderer
	sup      *Supervisor
	path     string
	log      *slog.Logger
	debounce time.Duration

	requests chan chan error
	// started is closed once loop is serving. Reload checks it so a request that
	// arrives before Start, or after the loop has stopped, fails immediately instead
	// of blocking on an unattended channel and hanging the caller's HTTP handler.
	started chan struct{}

	mu         sync.Mutex
	lastReload time.Time
	lastErr    error
	userCount  int
}

// NewManager wires a manager. It does not start anything.
func NewManager(cfg *config.Config, st *store.Store, log *slog.Logger) (*Manager, error) {
	r, err := NewRenderer(cfg)
	if err != nil {
		return nil, err
	}
	path := st.RenderedConfigPath()
	return &Manager{
		cfg:      cfg,
		store:    st,
		renderer: r,
		sup:      NewSupervisor(path, log),
		path:     path,
		log:      log,
		debounce: DefaultDebounce,
		requests: make(chan chan error),
		started:  make(chan struct{}),
	}, nil
}

// ConfigPath is where the rendered sing-box config lives.
func (m *Manager) ConfigPath() string { return m.path }

// Supervisor exposes the child process for status reporting.
func (m *Manager) Supervisor() *Supervisor { return m.sup }

// Start renders the config, validates it, launches sing-box and begins serving
// reload requests.
//
// Rendering happens before launch, and a render or validation failure aborts the
// start: booting with a stale config from a previous run would silently serve the
// wrong user list.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.render(ctx); err != nil {
		return err
	}
	if err := m.sup.Start(ctx); err != nil {
		return err
	}
	close(m.started)
	go m.loop(ctx)
	return nil
}

// ErrNotRunning is returned by Reload before Start or after shutdown.
var ErrNotRunning = errors.New("sing-box manager is not running")

// Reload regenerates the config and signals sing-box, coalescing with any other
// requests in the same debounce window.
//
// All callers that share a window get the same error, so an API caller whose change
// produced an invalid config still learns about it — the write already happened, but
// the reload result is honest about whether it took effect.
func (m *Manager) Reload(ctx context.Context) error {
	// Without this, a reload requested while no loop is serving would block until the
	// caller's context expired — an API request hanging for its full timeout instead
	// of reporting a clear error.
	select {
	case <-m.started:
	default:
		return ErrNotRunning
	}

	done := make(chan error, 1)
	select {
	case m.requests <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loop serialises reloads and applies the debounce window.
func (m *Manager) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case first := <-m.requests:
			waiters := []chan error{first}
			timer := time.NewTimer(m.debounce)

			// Gather everyone who asks during the window; they all want the same
			// thing, and the single render below will include all their changes.
		collect:
			for {
				select {
				case next := <-m.requests:
					waiters = append(waiters, next)
				case <-timer.C:
					break collect
				case <-ctx.Done():
					timer.Stop()
					for _, w := range waiters {
						w <- ctx.Err()
					}
					return
				}
			}

			err := m.apply(ctx)
			for _, w := range waiters {
				w <- err
			}
		}
	}
}

// apply renders, validates, installs and signals.
func (m *Manager) apply(ctx context.Context) error {
	if err := m.render(ctx); err != nil {
		return err
	}
	if err := m.sup.Reload(); err != nil {
		m.setLastErr(err)
		return err
	}
	m.mu.Lock()
	m.lastReload = time.Now()
	m.mu.Unlock()
	return nil
}

// render writes a validated config to disk, leaving the previous one in place on any
// failure.
func (m *Manager) render(ctx context.Context) error {
	// `vlessvmore identity set` writes identity.json from a separate process, so pick up
	// any change before rendering. Skipping this makes a rotation look like it applied
	// while the old key stays live until the next restart.
	if err := m.store.Identity.Reload(); err != nil {
		return m.fail(fmt.Errorf("reload identity: %w", err))
	}
	users, err := m.store.ActiveUsers(ctx, time.Now())
	if err != nil {
		return m.fail(fmt.Errorf("list active users: %w", err))
	}
	doc, err := m.renderer.Render(m.cfg, m.store.Identity.Get(), users)
	if err != nil {
		return m.fail(err)
	}
	// Validate before installing. The temp file goes in the target directory so the
	// check sees the same filesystem the real config will live on.
	if err := CheckBytes(ctx, filepath.Dir(m.path), doc); err != nil {
		return m.fail(err)
	}
	if err := writeAtomic(m.path, doc); err != nil {
		return m.fail(err)
	}

	m.mu.Lock()
	m.lastErr = nil
	m.userCount = len(users)
	m.mu.Unlock()
	m.log.Info("rendered sing-box config", "path", m.path, "active_users", len(users))
	return nil
}

func (m *Manager) fail(err error) error {
	m.setLastErr(err)
	m.log.Error("config generation failed; leaving the running config in place", "error", err)
	return err
}

func (m *Manager) setLastErr(err error) {
	m.mu.Lock()
	m.lastErr = err
	m.mu.Unlock()
}

// Status is a snapshot for the status endpoint.
type Status struct {
	Running     bool      `json:"running"`
	PID         int       `json:"pid,omitempty"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	ConfigPath  string    `json:"config_path"`
	ActiveUsers int       `json:"active_users"`
	LastReload  time.Time `json:"last_reload,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
}

// Status reports the current state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Status{
		Running:     m.sup.Running(),
		PID:         m.sup.PID(),
		StartedAt:   m.sup.StartedAt(),
		ConfigPath:  m.path,
		ActiveUsers: m.userCount,
		LastReload:  m.lastReload,
	}
	if m.lastErr != nil {
		s.LastError = m.lastErr.Error()
	}
	return s
}

// Stop shuts sing-box down. The context passed to Start should already be cancelled.
func (m *Manager) Stop(grace time.Duration) error {
	err := m.sup.Stop(grace)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
