package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vlessvmore/internal/reality"
	"vlessvmore/internal/store"
)

func newIdentityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Inspect or replace the server's Reality keypair",
		Long: `The Reality keypair identifies this server to its clients.

It is generated on first start and stored in the data directory, not in config.json —
it is produced, not chosen. Back up the data directory and the keypair comes with it.

Losing it means every client must be re-issued a link, so if a deployment ever comes up
with a new keypair while users already exist, that is logged as an error rather than
passed over quietly.`,
	}
	cmd.AddCommand(newIdentityShowCmd(), newIdentitySetCmd())
	return cmd
}

func newIdentityShowCmd() *cobra.Command {
	var (
		dataDir     string
		asJSON      bool
		showPrivate bool
	)
	cmd := &cobra.Command{
		Use:          "show",
		Short:        "Show the Reality public key and short id",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := store.Open(dataDir)
			if err != nil {
				return err
			}
			defer st.Close()

			id := st.Identity.Get()
			pub, err := st.Identity.PublicKey()
			if err != nil {
				return err
			}

			if asJSON {
				out := map[string]any{
					"public_key": pub,
					"short_id":   id.ShortID,
					"created_at": id.CreatedAt,
				}
				// Opt-in only: printing a private key by default would put it into shell
				// history, terminal scrollback and CI logs for anyone running `show`.
				if showPrivate {
					out["private_key"] = id.PrivateKey
				}
				return writeJSON(cmd.OutOrStdout(), out)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "public key\t%s\n", pub)
			fmt.Fprintf(tw, "short id\t%s\n", id.ShortID)
			fmt.Fprintf(tw, "created\t%s\n", id.CreatedAt.Format(time.RFC3339))
			if showPrivate {
				fmt.Fprintf(tw, "private key\t%s\n", id.PrivateKey)
			}
			return tw.Flush()
		},
	}
	f := cmd.Flags()
	f.StringVar(&dataDir, "data-dir", dataDirDefault(), "data directory to read")
	f.BoolVar(&showPrivate, "show-private-key", false, "also print the private key")
	f.BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newIdentitySetCmd() *cobra.Command {
	var (
		dataDir    string
		privateKey string
		shortID    string
		regenerate bool
		yes        bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Install a different Reality keypair",
		Long: `Install a different Reality keypair.

Two uses:

  1. Adopt an existing deployment's identity, so its clients keep working after a move:

       vlessvmore identity set --private-key <key> --short-id <id>

  2. Rotate to a fresh identity:

       vlessvmore identity set --regenerate

Rotating invalidates every existing client link at once — there is no overlap, because
sing-box accepts exactly one private key per inbound. Clients on a subscription URL pick
the new link up on their next poll; anyone holding a pasted link needs a new one.

Restart, or run ` + "`vlessvmore reload`" + `, to apply it.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if regenerate && (privateKey != "" || shortID != "") {
				return fmt.Errorf("--regenerate cannot be combined with --private-key or --short-id")
			}
			if !regenerate && privateKey == "" {
				return fmt.Errorf("pass --private-key (with optional --short-id) to adopt a keypair, or --regenerate for a fresh one")
			}

			st, err := store.Open(dataDir)
			if err != nil {
				return err
			}
			defer st.Close()

			current := st.Identity.Get()

			var next store.Identity
			if regenerate {
				fresh, err := store.NewIdentity(time.Now())
				if err != nil {
					return err
				}
				next = *fresh
			} else {
				if _, err := reality.DecodePrivateKey(privateKey); err != nil {
					return fmt.Errorf("--private-key: %w", err)
				}
				next = store.Identity{PrivateKey: privateKey, ShortID: shortID}
				if next.ShortID == "" {
					// Keep the existing short id when only the key is supplied: changing
					// both when the operator asked to change one would break clients for
					// a second, unrelated reason.
					next.ShortID = current.ShortID
				}
				if err := reality.ValidateShortID(next.ShortID); err != nil {
					return fmt.Errorf("--short-id: %w", err)
				}
			}

			changed := next.PrivateKey != current.PrivateKey || next.ShortID != current.ShortID
			if changed && !yes {
				n := len(st.Users.List())
				if !confirm(cmd, fmt.Sprintf(
					"Replace the Reality identity? All %d user(s) will need a new link.", n)) {
					fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
					return nil
				}
			}

			if err := st.Identity.Replace(next); err != nil {
				return err
			}
			pub, err := st.Identity.PublicKey()
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "public key  %s\n", pub)
			fmt.Fprintf(out, "short id    %s\n", next.ShortID)
			if !changed {
				fmt.Fprintln(cmd.ErrOrStderr(), "\nidentity unchanged; nothing to do")
				return nil
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"\nRun `vlessvmore reload` (or restart) to apply it, then re-issue links.\n"+
					"Clients on a subscription URL update themselves on their next poll.\n")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&dataDir, "data-dir", dataDirDefault(), "data directory to write")
	f.StringVar(&privateKey, "private-key", "", "Reality private key to adopt")
	f.StringVar(&shortID, "short-id", "", "Reality short id to adopt (default: keep the current one)")
	f.BoolVar(&regenerate, "regenerate", false, "generate a fresh keypair instead of adopting one")
	f.BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}
