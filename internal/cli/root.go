// Package cli implements the vlessvmore command line.
//
// Every command except `init` and `serve` is a thin client over the daemon's unix
// socket, so the CLI and the HTTP API cannot drift apart: they are the same code paths
// reached through the same routes.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// clientKey carries the socket client through cobra's context.
type clientKey struct{}

// Execute runs the CLI, applying argv[0] dispatch first.
//
// The image symlinks `user` and `token` to the binary, so `docker exec vlessvmore user
// add alice` works: when argv[0] is not "vlessvmore", it is prepended as the first
// command word.
func Execute() int {
	args := os.Args[1:]
	if base := filepath.Base(os.Args[0]); base != "vlessvmore" && base != "vlessvmore.test" {
		args = append([]string{base}, args...)
	}

	root := NewRootCmd()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command {
	var socket string

	root := &cobra.Command{
		Use:   "vlessvmore",
		Short: "A self-contained sing-box VLESS/Reality server with a management API",
		Long: `vlessvmore runs sing-box with a VLESS/Reality inbound and manages its users.

Getting started:

    vlessvmore init --host vpn.example.com > config/config.json   # generate a config
    docker compose up -d                                         # run it
    vlessvmore user add alice                                    # add a user, get a
                                                                 # link and a sub URL

The sing-box config is generated from config.json plus the user list every time
anything changes. Do not edit it by hand — the next change overwrites it.`,
		SilenceErrors: true,
		// Commands report their own errors; usage on a runtime failure is noise.
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVar(&socket, "socket", SocketPath(),
		"path to the manager unix socket")

	// Attach a client lazily so `init` and `serve`, which need no daemon, do not
	// require the socket to exist.
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		ctx := cmd.Context()
		cmd.SetContext(withClient(ctx, NewClient(socket)))
	}

	root.AddCommand(
		newInitCmd(),
		newServeCmd(),
		newRenderCmd(),
		newUserCmd(),
		newTokenCmd(),
		newIdentityCmd(),
		newServerCmd(),
		newStatusCmd(),
		newReloadCmd(),
		newExportCmd(),
		newImportCmd(),
		newVersionCmd(),
	)
	return root
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
