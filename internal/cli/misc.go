package cli

import (
	"fmt"
	"os"
	"runtime/debug"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vlessvmore/internal/api"
	"vlessvmore/internal/config"
	"vlessvmore/internal/singbox"
	"vlessvmore/internal/store"
)

// Version is overridable at build time with
// -ldflags "-X vlessvmore/internal/cli.Version=v1.2.3".
var Version = "dev"

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "version",
		Short:        "Print version information, including sing-box's build tags",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "vlessvmore %s\n", versionString())

			// sing-box's build tags are the interesting part: without with_v2ray_api
			// there are no per-user counters, and that is invisible until usage stays
			// stubbornly at zero.
			v, err := singbox.Version(cmd.Context())
			if err != nil {
				fmt.Fprintf(out, "sing-box: not available (%v)\n", err)
				return nil
			}
			fmt.Fprintf(out, "\n%s\n", v)
			if ok, _ := singbox.HasV2RayAPI(cmd.Context()); !ok {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"\nwarning: this sing-box was built without with_v2ray_api;\n"+
						"         per-user traffic stats and quota enforcement will not work\n")
			}
			return nil
		},
	}
}

func versionString() string {
	if Version != "dev" {
		return Version
	}
	// A `go install`ed binary knows its module version even without ldflags.
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return Version
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show service status",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var resp api.StatusResponse
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/status", nil, &resp); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), resp)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			state := "stopped"
			if resp.SingBox.Running {
				state = fmt.Sprintf("running (pid %d)", resp.SingBox.PID)
			}
			fmt.Fprintf(tw, "sing-box\t%s\n", state)
			if !resp.SingBox.StartedAt.IsZero() {
				fmt.Fprintf(tw, "uptime\t%s\n", time.Since(resp.SingBox.StartedAt).Truncate(time.Second))
			}
			fmt.Fprintf(tw, "users\t%d (%d active)\n", resp.Users, resp.ActiveUsers)
			fmt.Fprintf(tw, "api tokens\t%d\n", resp.Tokens)
			fmt.Fprintf(tw, "data dir\t%s\n", resp.DataDir)
			fmt.Fprintf(tw, "rendered config\t%s\n", resp.SingBox.ConfigPath)
			if !resp.SingBox.LastReload.IsZero() {
				fmt.Fprintf(tw, "last reload\t%s\n", resp.SingBox.LastReload.Format(time.RFC3339))
			}
			if resp.SingBox.LastError != "" {
				fmt.Fprintf(tw, "last error\t%s\n", resp.SingBox.LastError)
			}
			tw.Flush()
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newReloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Regenerate the sing-box config and reload it",
		Long: `Regenerate the sing-box config and reload it.

Reloading drops established connections; clients reconnect within a second or so. This
is inherent to sing-box having no runtime user API, which is why reloads that happen
close together are coalesced into one.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var st singbox.Status
			if err := client(cmd).Do(cmd.Context(), "POST", "/api/reload", nil, &st); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reloaded: %d active user(s)\n", st.ActiveUsers)
			return nil
		},
	}
}

func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Show server connection details",
	}
	var asJSON bool
	show := &cobra.Command{
		Use:          "show",
		Short:        "Show the host, SNI and Reality public key clients need",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var resp api.ServerResponse
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/server", nil, &resp); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), resp)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "host\t%s:%d\n", resp.Host, resp.Port)
			fmt.Fprintf(tw, "sni\t%s\n", resp.SNI)
			fmt.Fprintf(tw, "handshake\t%s\n", resp.Handshake)
			fmt.Fprintf(tw, "public key\t%s\n", resp.PublicKey)
			fmt.Fprintf(tw, "short id\t%s\n", resp.ShortID)
			if resp.Flow != "" {
				fmt.Fprintf(tw, "flow\t%s\n", resp.Flow)
			}
			fmt.Fprintf(tw, "fingerprint\t%s\n", resp.Fingerprint)
			tw.Flush()
			return nil
		},
	}
	show.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	cmd.AddCommand(show)
	return cmd
}

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "token",
		Short:   "Manage API tokens",
		Aliases: []string{"tokens"},
	}

	var raw bool
	create := &cobra.Command{
		Use:   "create <label>",
		Short: "Mint an API token",
		Long: `Mint an API token.

The secret is shown once and never again — only its hash is stored, so a leaked
tokens.json cannot be replayed. With --raw only the secret is printed, for
  TOKEN=$(vlessvmore token create web --raw)`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Token  store.Token `json:"token"`
				Secret string      `json:"secret"`
			}
			if err := client(cmd).Do(cmd.Context(), "POST", "/api/tokens",
				api.CreateTokenRequest{Label: args[0]}, &resp); err != nil {
				return err
			}
			if raw {
				fmt.Fprintln(cmd.OutOrStdout(), resp.Secret)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", resp.Secret)
			fmt.Fprintf(cmd.ErrOrStderr(), "\nlabel  %s\nid     %s\n\nSave it now — it cannot be shown again.\n",
				resp.Token.Label, resp.Token.ID)
			return nil
		},
	}
	create.Flags().BoolVar(&raw, "raw", false, "print only the secret, for capture into a variable")

	var asJSON bool
	list := &cobra.Command{
		Use:          "ls",
		Short:        "List API tokens",
		Aliases:      []string{"list"},
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var resp struct {
				Tokens []store.Token `json:"tokens"`
			}
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/tokens", nil, &resp); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), resp.Tokens)
			}
			if len(resp.Tokens) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no tokens yet — create one with `vlessvmore token create <label>`")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "LABEL\tID\tCREATED\tLAST USED\tSTATE")
			for _, t := range resp.Tokens {
				last := "never"
				if t.LastUsedAt != nil {
					last = t.LastUsedAt.Format("2006-01-02 15:04")
				}
				state := "active"
				if t.RevokedAt != nil {
					state = "revoked"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					t.Label, t.ID, t.CreatedAt.Format("2006-01-02"), last, state)
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	remove := &cobra.Command{
		Use:          "rm <id|label>",
		Short:        "Delete an API token",
		Aliases:      []string{"remove", "delete", "revoke"},
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client(cmd).Do(cmd.Context(), "DELETE", "/api/tokens/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(create, list, remove)
	return cmd
}

// newRenderCmd prints the sing-box config that the current config.json and user list
// would produce. It needs no daemon, which makes the template independently testable.
func newRenderCmd() *cobra.Command {
	var configPath, dataDir string
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Print the sing-box config that would be generated",
		Long: `Print the sing-box config that would be generated, without installing it.

Useful for inspecting the template's output or piping into sing-box check:

    vlessvmore render | sing-box check -c /dev/stdin`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			st, err := store.Open(dataDir)
			if err != nil {
				return err
			}
			defer st.Close()

			users, err := st.ActiveUsers(cmd.Context(), time.Now())
			if err != nil {
				return err
			}
			r, err := singbox.NewRenderer(cfg)
			if err != nil {
				return err
			}
			doc, err := r.Render(cfg, st.Identity.Get(), users)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(doc)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&configPath, "config", configDefault(), "path to config.json")
	f.StringVar(&dataDir, "data-dir", dataDirDefault(), "path to the data directory")
	return cmd
}

func configDefault() string {
	if v := os.Getenv(config.PathEnv); v != "" {
		return v
	}
	return config.DefaultPath
}

func dataDirDefault() string {
	if v := os.Getenv(store.DirEnv); v != "" {
		return v
	}
	return store.DefaultDir
}
