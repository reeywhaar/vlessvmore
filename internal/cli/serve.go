package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"vlessvmore/internal/api"
	"vlessvmore/internal/backup"
	"vlessvmore/internal/config"
	"vlessvmore/internal/singbox"
	"vlessvmore/internal/stats"
	"vlessvmore/internal/store"
)

// shutdownGrace is how long sing-box and the HTTP servers get to stop cleanly.
const shutdownGrace = 10 * time.Second

func newServeCmd() *cobra.Command {
	var configPath, dataDir, socketPath string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the manager and sing-box (the container entrypoint)",
		Long: `Run sing-box under a supervisor, serve the management API, and collect traffic stats.

This is what the container runs by default. It renders the sing-box config from
config.json plus the user list, validates it, starts sing-box, and then keeps the two in
sync as users change.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd, configPath, dataDir, socketPath)
		},
	}
	f := cmd.Flags()
	f.StringVar(&configPath, "config", configDefault(), "path to config.json")
	f.StringVar(&dataDir, "data-dir", dataDirDefault(), "data directory (should be a volume)")
	f.StringVar(&socketPath, "listen-socket", SocketPath(), "unix socket for the CLI")
	return cmd
}

func serve(cmd *cobra.Command, configPath, dataDir, socketPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
		Level: logLevel(cfg.LogLevel),
	}))

	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	// A freshly generated identity on a deployment that already has users means the data
	// directory was partly lost, and every client is about to fail. Say so loudly.
	st.WarnOnUnexpectedIdentity(log)

	// Warn rather than fail: the proxy works fine without stats, and refusing to
	// start would turn a metering problem into an outage.
	if ok, err := singbox.HasV2RayAPI(cmd.Context()); err == nil && !ok {
		log.Warn("sing-box was built without with_v2ray_api; " +
			"per-user traffic stats and quota enforcement will not work")
	}

	mgr, err := singbox.NewManager(cfg, st, log)
	if err != nil {
		return err
	}

	// Signals are trapped before anything starts so a Ctrl-C during startup still
	// shuts down in order rather than orphaning sing-box.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mgr.Start(ctx); err != nil {
		return err
	}
	defer func() {
		if err := mgr.Stop(shutdownGrace); err != nil {
			log.Error("stopping sing-box", "error", err)
		}
	}()

	collector, err := stats.New(singbox.StatsListen, st, mgr, time.Duration(cfg.StatsInterval), log)
	if err != nil {
		return err
	}
	defer collector.Close()
	go collector.Run(ctx)

	// Copies of the deployment, when the mode says one is due. Off unless an address was
	// named — see internal/backup for why this program pushes rather than being pulled from.
	go (&backup.Pusher{
		Source: backup.Source{Store: st, ConfigPath: configPath, Log: log},
		URL:    cfg.BackupURL,
		Mode:   cfg.BackupMode,
	}).Run(ctx)

	server := api.New(cfg, st, mgr, log)

	// Two listeners, two trust levels: the socket is unauthenticated because reaching
	// it already means root in this container, the TCP port always requires a token.
	socketSrv, err := listenSocket(socketPath, server.Handler(false), log)
	if err != nil {
		return err
	}
	defer socketSrv.Close()

	tcpSrv := &http.Server{
		Addr:              cfg.APIListen,
		Handler:           server.Handler(true),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Buffered, so a listener that stops after the select has already returned is not
	// left blocking on a channel nobody reads again.
	tcpErr := make(chan error, 1)
	go func() {
		log.Info("management api listening", "addr", cfg.APIListen, "socket", socketPath)
		if err := tcpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			tcpErr <- err
			return
		}
		tcpErr <- nil
	}()

	log.Info("vlessvmore started",
		"host", cfg.Host, "port", cfg.Port, "data_dir", dataDir, "config", configPath)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-tcpErr:
		if err != nil {
			return fmt.Errorf("management api: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := tcpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutting down the management api", "error", err)
	}
	return nil
}

// listenSocket serves handler on a unix socket, replacing any stale socket file left
// by a previous unclean exit.
func listenSocket(path string, handler http.Handler, log *slog.Logger) (*http.Server, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	// A crash leaves the socket file behind, and bind would then fail forever. It is
	// safe to remove: if a live process still held it, we would fail on the port bind
	// instead.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// Owner only: the socket is unauthenticated, so filesystem permissions are the
	// access control.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}

	srv := &http.Server{Handler: handler}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("cli socket server stopped", "error", err)
		}
	}()
	return srv, nil
}

func logLevel(s string) slog.Level {
	switch s {
	case "trace", "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error", "fatal", "panic":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
