package cli

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"vlessvmore/internal/config"
)

func newInitCmd() *cobra.Command {
	var (
		host        string
		sni         string
		handshake   string
		port        int
		flow        string
		fingerprint string
		name        string
		subBase     string
	)

	cmd := &cobra.Command{
		Use:   "init --host <hostname>",
		Short: "Generate a config.json on stdout",
		Long: `Generate a new config.json and write it to stdout.

The config is the only thing on stdout, so redirect it to a file:

    vlessvmore init --host vpn.example.com > config/config.json

Anything meant for a human goes to stderr, so it stays visible on the terminal while the
redirect captures clean JSON.

config.json contains no secrets — it is just where the server lives and how it behaves,
so it is safe to commit or template. The Reality keypair is generated on first start and
kept in the data directory instead; see ` + "`vlessvmore identity`" + `.

This command is standalone: no daemon, no data directory, no network.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

			if sni == "" {
				sni = host
			}
			hsHost, hsPort, err := splitHandshake(handshake, sni)
			if err != nil {
				return err
			}

			cfg := &config.Config{
				Version:             config.Version,
				Name:                name,
				Host:                host,
				Port:                port,
				SNI:                 sni,
				Handshake:           config.Handshake{Server: hsHost, ServerPort: hsPort},
				Flow:                &flow,
				Fingerprint:         fingerprint,
				SubscriptionURLBase: subBase,
				APIListen:           config.DefaultAPIListen,
				BackupMode:          config.DefaultBackupMode,
				LogLevel:            config.DefaultLogLevel,
			}
			cfg.StatsInterval = config.Duration(config.DefaultStatsInterval)

			// Validate what we are about to emit, so `init` can never produce a file
			// that `serve` will refuse.
			if err := cfg.Validate(); err != nil {
				return err
			}

			doc, err := cfg.Marshal()
			if err != nil {
				return err
			}
			if _, err := out.Write(doc); err != nil {
				return err
			}

			writeInitReport(errOut, cfg)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&host, "host", "", "public hostname clients dial (required)")
	f.StringVar(&sni, "sni", "", "Reality server_name (default: --host)")
	f.StringVar(&handshake, "handshake", "", "Reality handshake target host[:port] (default: <sni>:443)")
	f.IntVar(&port, "port", config.DefaultPort, "port the vless inbound listens on")
	f.StringVar(&flow, "flow", config.DefaultFlow, `vless flow; pass "" for plain vless without vision`)
	f.StringVar(&fingerprint, "fingerprint", config.DefaultFingerprint, "uTLS fingerprint hint for clients")
	f.StringVar(&name, "name", "",
		"name clients display for this server (default: each user's own name)")
	f.StringVar(&subBase, "subscription-url-base", "",
		"public origin clients fetch subscriptions from (default: https://<host>)")
	_ = cmd.MarkFlagRequired("host")

	return cmd
}

// splitHandshake parses a "host:port" handshake target, defaulting the port to 443 and
// the host to the SNI.
func splitHandshake(v, sni string) (string, int, error) {
	if v == "" {
		return sni, config.DefaultHandshakePort, nil
	}
	// A bare hostname is the common case and should not require ":443".
	if !strings.Contains(v, ":") {
		return v, config.DefaultHandshakePort, nil
	}
	h, p, err := net.SplitHostPort(v)
	if err != nil {
		return "", 0, fmt.Errorf("--handshake %q must be host or host:port: %w", v, err)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("--handshake %q: port must be a number in 1-65535", v)
	}
	if h == "" {
		h = sni
	}
	return h, n, nil
}

// writeInitReport prints the human half of init to stderr.
func writeInitReport(w io.Writer, cfg *config.Config) {
	fmt.Fprintf(w, "\nServer\n")
	if cfg.Name != "" {
		fmt.Fprintf(w, "  Name          %s   (shown in clients)\n", cfg.Name)
	}
	fmt.Fprintf(w, "  Host          %s:%d\n", cfg.Host, cfg.Port)
	fmt.Fprintf(w, "  SNI           %s\n", cfg.SNI)
	fmt.Fprintf(w, "  Handshake     %s:%d\n", cfg.Handshake.Server, cfg.Handshake.ServerPort)
	fmt.Fprintf(w, "  Subscriptions %s/sub/<token>\n", cfg.SubscriptionBase())
	fmt.Fprintf(w, "\nThis file holds no secrets. The Reality keypair is generated on first\n")
	fmt.Fprintf(w, "start and stored in the data directory — back that up, not this.\n")

	fmt.Fprintf(w, `
Next steps
  1. Point %s at this host with a DNS A record. No CDN proxy — Reality needs
     the TLS handshake to reach this server directly.
  2. Make sure %s serves a real certificate for %s;
     that is the handshake Reality borrows.
  3. Mount the config and a data directory, then start it:
       volumes:
         - ./config:/etc/vlessvmore:ro
         - ./data:/var/lib/vlessvmore
       docker compose up -d
  4. Add a user and hand out their subscription URL:
       docker exec vlessvmore vlessvmore user add alice
`, cfg.Host, cfg.Handshake.Server, cfg.SNI)
}
