package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"vlessvmore/internal/store"
)

func newExportCmd() *cobra.Command {
	var (
		dataDir string
		usage   bool
		tokens  bool
		all     bool
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write all users (and optionally usage and tokens) to stdout as JSON",
		Long: `Write the deployment's state to stdout as a single JSON document.

    vlessvmore export --all > backup.json

This is the supported way to back up or move a deployment. Copying the data directory
by hand is not equivalent: stats.db is SQLite in WAL mode, so a live file copy can
capture a torn database unless its -wal and -shm files come along in the right order.
Going through this command avoids that entirely and is safe while the service runs.

The dump contains user UUIDs, which are credentials. Treat the file as a secret.

Runs directly against the data directory, so it works whether or not the daemon is up.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := store.Open(dataDir)
			if err != nil {
				return err
			}
			defer st.Close()

			opts := store.ExportOptions{
				IncludeUsage:  usage || all,
				IncludeTokens: tokens || all,
			}
			dump, err := st.Export(cmd.Context(), time.Now(), opts)
			if err != nil {
				return err
			}
			if err := dump.Encode(cmd.OutOrStdout()); err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "exported %d user(s)", len(dump.Users))
			if opts.IncludeTokens {
				fmt.Fprintf(cmd.ErrOrStderr(), ", %d token(s)", len(*dump.Tokens))
			}
			if opts.IncludeUsage {
				fmt.Fprintf(cmd.ErrOrStderr(), ", %d usage bucket(s)", len(*dump.Usage))
			}
			fmt.Fprintln(cmd.ErrOrStderr())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&dataDir, "data-dir", dataDirDefault(), "data directory to read")
	f.BoolVar(&usage, "usage", false, "include traffic history")
	f.BoolVar(&tokens, "tokens", false, "include API tokens (they belong to the old host)")
	f.BoolVar(&all, "all", false, "include everything: usage and tokens")
	return cmd
}

func newImportCmd() *cobra.Command {
	var (
		dataDir string
		file    string
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Restore users, usage and tokens from an export",
		Long: `Restore a deployment from a dump produced by ` + "`vlessvmore export`" + `.

    vlessvmore import < backup.json

Refuses to run when the target already has users, because importing replaces them all.
Pass --force to overwrite deliberately.

Sections absent from the dump are left alone: importing a users-only export does not
wipe existing usage history.

Restart the service afterwards, or run ` + "`vlessvmore reload`" + `, so the imported users
reach the sing-box config.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := cmd.InOrStdin()
			if file != "" {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer f.Close()
				in = f
			}

			dump, err := store.ReadDump(in)
			if err != nil {
				return err
			}

			st, err := store.Open(dataDir)
			if err != nil {
				return err
			}
			defer st.Close()

			if err := st.Import(cmd.Context(), dump, force); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "imported %d user(s)", len(dump.Users))
			if dump.Tokens != nil {
				fmt.Fprintf(out, ", %d token(s)", len(*dump.Tokens))
			}
			if dump.Usage != nil {
				fmt.Fprintf(out, ", %d usage bucket(s)", len(*dump.Usage))
			}
			fmt.Fprintf(out, "\nrun `vlessvmore reload` (or restart) to apply them\n")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&dataDir, "data-dir", dataDirDefault(), "data directory to write")
	f.StringVar(&file, "file", "", "read from this file instead of stdin")
	f.BoolVar(&force, "force", false, "overwrite existing users")
	return cmd
}
