package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vlessvmore/internal/api"
)

func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "user",
		Short:   "Manage users",
		Aliases: []string{"users"},
	}
	cmd.AddCommand(
		newUserAddCmd(),
		newUserListCmd(),
		newUserShowCmd(),
		newUserSetCmd(),
		newUserRemoveCmd(),
		newUserLinkCmd(),
		newUserUsageCmd(),
		newUserResetUsageCmd(),
		newUserSubCmd(),
		newUserRotateSubCmd(),
	)
	return cmd
}

// changeResponse is what the API returns for a mutation: the object plus whether the
// sing-box reload that followed it succeeded.
type changeResponse struct {
	Result      api.UserResponse `json:"result"`
	Reloaded    bool             `json:"reloaded"`
	ReloadError string           `json:"reload_error,omitempty"`
}

// report prints the reload outcome. A failed reload is not a failed command — the
// change is saved — but the operator has to know it is not live yet.
func (c changeResponse) report(w io.Writer) {
	if !c.Reloaded {
		fmt.Fprintf(w, "warning: saved, but sing-box was not reloaded: %s\n", c.ReloadError)
		fmt.Fprintf(w, "         the change takes effect on the next successful reload\n")
	}
}

func newUserAddCmd() *cobra.Command {
	var (
		uuid     string
		quota    string
		expires  string
		disabled bool
		note     string
		asJSON   bool
		qr       bool
	)
	cmd := &cobra.Command{
		Use:          "add <name>",
		Short:        "Create a user",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req := api.CreateUserRequest{Name: args[0], UUID: uuid, Note: note}
			if quota != "" {
				n, err := ParseBytes(quota)
				if err != nil {
					return fmt.Errorf("--quota: %w", err)
				}
				req.QuotaBytes = n
			}
			if expires != "" {
				t, err := ParseTime(expires, time.Now())
				if err != nil {
					return fmt.Errorf("--expires: %w", err)
				}
				req.ExpiresAt = &t
			}
			if disabled {
				no := false
				req.Enabled = &no
			}

			var resp changeResponse
			if err := client(cmd).Do(cmd.Context(), "POST", "/api/users", req, &resp); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), resp.Result)
			}
			resp.report(cmd.ErrOrStderr())
			printUserDetail(cmd.OutOrStdout(), resp.Result)

			// The link is the thing the operator actually needs next, so fetch it
			// rather than making them run a second command.
			var link api.LinkResponse
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/users/"+resp.Result.ID+"/link", nil, &link); err == nil {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "\n%s\n", link.Link)
				// The QR encodes the subscription URL rather than the link when one
				// exists: a client that subscribes keeps working through a key rotation
				// or a port change, where a scanned static link would not.
				target := link.Link
				if link.SubscriptionURL != "" {
					target = link.SubscriptionURL
				}
				if qr {
					errOut := cmd.ErrOrStderr()
					fmt.Fprintln(errOut)
					printQR(errOut, target)
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&uuid, "uuid", "", "use this UUID instead of generating one")
	f.StringVar(&quota, "quota", "", "traffic limit, e.g. 100GB (default: unlimited)")
	f.StringVar(&expires, "expires", "", "expiry: 2026-12-31, an RFC3339 timestamp, or a duration like 30d")
	f.BoolVar(&disabled, "disabled", false, "create the user without enabling them")
	f.StringVar(&note, "note", "", "free-form note")
	f.BoolVar(&qr, "qr", true, "draw a QR code; --qr=false to suppress it")
	f.BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newUserListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "ls",
		Short:        "List users",
		Aliases:      []string{"list"},
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var resp struct {
				Users []api.UserResponse `json:"users"`
			}
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/users?include=usage", nil, &resp); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), resp.Users)
			}
			if len(resp.Users) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no users yet — create one with `vlessvmore user add <name>`")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATE\tUSED\tQUOTA\tEXPIRES\tNOTE")
			for _, u := range resp.Users {
				used, quota := "-", "unlimited"
				if u.Usage != nil {
					used = FormatBytes(u.Usage.WindowTotal)
					if u.QuotaBytes > 0 {
						quota = FormatBytes(u.QuotaBytes)
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					u.Name, userState(u), used, quota, expiryText(u), u.Note)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newUserShowCmd() *cobra.Command {
	var (
		asJSON bool
		qr     bool
	)
	cmd := &cobra.Command{
		Use:          "show <name|id>",
		Short:        "Show one user in detail, with their link",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var u api.UserResponse
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/users/"+args[0], nil, &u); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), u)
			}
			out := cmd.OutOrStdout()
			printUserDetail(out, u)

			// Best effort: the details are worth printing even if the link cannot be
			// built, which needs the server config rather than just the user.
			var link api.LinkResponse
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/users/"+u.ID+"/link", nil, &link); err == nil {
				fmt.Fprintf(out, "\n%s\n", link.Link)
				target := link.Link
				if link.SubscriptionURL != "" {
					target = link.SubscriptionURL
				}
				if qr {
					errOut := cmd.ErrOrStderr()
					fmt.Fprintln(errOut)
					printQR(errOut, target)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&qr, "qr", true, "draw a QR code; --qr=false to suppress it")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newUserSetCmd() *cobra.Command {
	var (
		name    string
		uuid    string
		quota   string
		expires string
		enable  bool
		disable bool
		note    string
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "set <name|id>",
		Short: "Change a user",
		Long: `Change a user. Only the flags you pass are altered.

Pass --expires "" to remove an expiry, and --quota 0 to make a user unlimited.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if enable && disable {
				return fmt.Errorf("--enable and --disable are mutually exclusive")
			}

			// Built as a map so an explicit `--expires ""` can send JSON null, which is
			// how the API distinguishes "clear it" from "leave it alone".
			body := map[string]any{}
			f := cmd.Flags()
			if f.Changed("name") {
				body["name"] = name
			}
			if f.Changed("uuid") {
				body["uuid"] = uuid
			}
			if f.Changed("note") {
				body["note"] = note
			}
			if f.Changed("quota") {
				n, err := ParseBytes(quota)
				if err != nil {
					return fmt.Errorf("--quota: %w", err)
				}
				body["quota_bytes"] = n
			}
			if f.Changed("expires") {
				if strings.TrimSpace(expires) == "" {
					body["expires_at"] = nil
				} else {
					t, err := ParseTime(expires, time.Now())
					if err != nil {
						return fmt.Errorf("--expires: %w", err)
					}
					body["expires_at"] = t
				}
			}
			if enable {
				body["enabled"] = true
			}
			if disable {
				body["enabled"] = false
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change; pass at least one flag (see --help)")
			}

			var resp changeResponse
			if err := client(cmd).Do(cmd.Context(), "PATCH", "/api/users/"+args[0], body, &resp); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), resp.Result)
			}
			resp.report(cmd.ErrOrStderr())
			printUserDetail(cmd.OutOrStdout(), resp.Result)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "rename the user; usage history is kept")
	f.StringVar(&uuid, "uuid", "", "replace the UUID; existing clients stop working")
	f.StringVar(&quota, "quota", "", "traffic limit, e.g. 100GB; 0 for unlimited")
	f.StringVar(&expires, "expires", "", `expiry date, or "" to remove it`)
	f.BoolVar(&enable, "enable", false, "enable the user")
	f.BoolVar(&disable, "disable", false, "disable the user")
	f.StringVar(&note, "note", "", "free-form note")
	f.BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newUserRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:          "rm <name|id>",
		Short:        "Delete a user and their usage history",
		Aliases:      []string{"remove", "delete"},
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deleting drops the usage history too and cannot be undone, so it asks
			// unless told not to.
			if !yes {
				if !confirm(cmd, fmt.Sprintf("Delete user %q and all of their usage history?", args[0])) {
					fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
					return nil
				}
			}
			var resp struct {
				Result struct {
					Deleted string `json:"deleted"`
					Name    string `json:"name"`
				} `json:"result"`
				Reloaded    bool   `json:"reloaded"`
				ReloadError string `json:"reload_error,omitempty"`
			}
			if err := client(cmd).Do(cmd.Context(), "DELETE", "/api/users/"+args[0], nil, &resp); err != nil {
				return err
			}
			if !resp.Reloaded {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: deleted, but sing-box was not reloaded: %s\n", resp.ReloadError)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s (%s)\n", resp.Result.Name, resp.Result.Deleted)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func newUserLinkCmd() *cobra.Command {
	var qr bool
	cmd := &cobra.Command{
		Use:   "link <name|id>",
		Short: "Print a user's vless:// link",
		Long: `Print a user's vless:// link.

By default the link is the only thing on stdout, even on a terminal, so it stays
pipeable and safe to capture:

    LINK=$(vlessvmore user link alice)

A QR code is drawn on stderr as well; --qr=false suppresses it.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Link string `json:"link"`
			}
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/users/"+args[0]+"/link", nil, &resp); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), resp.Link)
			// Opt-in here, unlike add/show/sub: `link` exists to emit exactly the URI,
			// and a static link is the thing you reach for when a QR is not the point.
			if qr {
				errOut := cmd.ErrOrStderr()
				fmt.Fprintln(errOut)
				printQR(errOut, resp.Link)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&qr, "qr", false, "also draw a QR code")
	return cmd
}

func newUserUsageCmd() *cobra.Command {
	var (
		days   int
		bucket string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:          "usage <name|id>",
		Short:        "Show a user's traffic history",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if bucket != "hour" && bucket != "day" {
				return fmt.Errorf("--bucket must be hour or day, got %q", bucket)
			}
			from := time.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339)
			path := fmt.Sprintf("/api/users/%s/usage?bucket=%s&from=%s", args[0], bucket, from)

			var resp struct {
				Name   string `json:"name"`
				Series []struct {
					Bucket time.Time `json:"bucket"`
					Up     int64     `json:"up"`
					Down   int64     `json:"down"`
				} `json:"series"`
				Summary api.UsageSummary `json:"summary"`
			}
			if err := client(cmd).Do(cmd.Context(), "GET", path, nil, &resp); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), resp)
			}

			out := cmd.OutOrStdout()
			layout := "2006-01-02 15:04"
			if bucket == "day" {
				layout = "2006-01-02"
			}
			if len(resp.Series) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "no traffic recorded for %s in the last %d day(s)\n", resp.Name, days)
			} else {
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "WHEN\tUP\tDOWN\tTOTAL")
				for _, p := range resp.Series {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Bucket.Format(layout),
						FormatBytes(p.Up), FormatBytes(p.Down), FormatBytes(p.Up+p.Down))
				}
				tw.Flush()
			}
			fmt.Fprintf(out, "\nsince quota reset  %s\n", FormatBytes(resp.Summary.WindowTotal))
			if resp.Summary.QuotaBytes > 0 {
				fmt.Fprintf(out, "quota              %s (%s remaining)\n",
					FormatBytes(resp.Summary.QuotaBytes), FormatBytes(resp.Summary.QuotaRemaining))
			}
			fmt.Fprintf(out, "lifetime           %s\n", FormatBytes(resp.Summary.Total))
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVar(&days, "days", 7, "how many days back to show")
	f.StringVar(&bucket, "bucket", "day", "granularity: hour or day")
	f.BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newUserResetUsageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset-usage <name|id>",
		Short: "Start a new quota window for a user",
		Long: `Start a new quota window, which re-enables a user disabled for exceeding their quota.

History is not deleted: the traffic still happened, it is just no longer counted
against the current quota.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp changeResponse
			if err := client(cmd).Do(cmd.Context(), "POST", "/api/users/"+args[0]+"/reset-usage", nil, &resp); err != nil {
				return err
			}
			resp.report(cmd.ErrOrStderr())
			printUserDetail(cmd.OutOrStdout(), resp.Result)
			return nil
		},
	}
	return cmd
}

func newUserSubCmd() *cobra.Command {
	var qr bool
	cmd := &cobra.Command{
		Use:   "sub <name|id>",
		Short: "Print a user's subscription URL",
		Long: `Print the subscription URL to give a client.

Prefer this over a raw link: the client re-fetches it periodically, so it picks up a
rotated key or a changed port on its own, and it reads the traffic and expiry headers the
server sends to show quota remaining in its own UI.

The URL is a capability — anyone holding it can fetch the credential — so treat it like
the link itself. ` + "`user rotate-sub`" + ` invalidates it without disturbing the user's UUID.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var u api.UserResponse
			if err := client(cmd).Do(cmd.Context(), "GET", "/api/users/"+args[0], nil, &u); err != nil {
				return err
			}
			if u.SubscriptionURL == "" {
				return fmt.Errorf("user %q has no subscription token", u.Name)
			}
			fmt.Fprintln(cmd.OutOrStdout(), u.SubscriptionURL)
			// Stderr, so the QR never lands in `SUB=$(vlessvmore user sub alice)`.
			if qr {
				errOut := cmd.ErrOrStderr()
				fmt.Fprintln(errOut)
				printQR(errOut, u.SubscriptionURL)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&qr, "qr", true, "draw a QR code; --qr=false to suppress it")
	return cmd
}

func newUserRotateSubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate-sub <name|id>",
		Short: "Issue a new subscription URL, invalidating the old one",
		Long: `Issue a new subscription URL for a user.

The old URL stops working immediately. The user's UUID is untouched, so an
already-configured client keeps connecting — this cuts off a leaked subscription link
without disconnecting anyone.`,
		Aliases:      []string{"sub-rotate"},
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var u api.UserResponse
			if err := client(cmd).Do(cmd.Context(), "POST", "/api/users/"+args[0]+"/rotate-sub", nil, &u); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), u.SubscriptionURL)
			fmt.Fprintln(cmd.ErrOrStderr(), "\nthe previous subscription URL no longer works")
			return nil
		},
	}
	return cmd
}

func userState(u api.UserResponse) string {
	if u.Enabled {
		return "enabled"
	}
	if u.DisabledReason != "" {
		return "disabled:" + u.DisabledReason
	}
	return "disabled"
}

func expiryText(u api.UserResponse) string {
	if u.ExpiresAt == nil {
		return "never"
	}
	return u.ExpiresAt.Format("2006-01-02")
}

func printUserDetail(w io.Writer, u api.UserResponse) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "name\t%s\n", u.Name)
	fmt.Fprintf(tw, "id\t%s\n", u.ID)
	fmt.Fprintf(tw, "uuid\t%s\n", u.UUID)
	fmt.Fprintf(tw, "state\t%s\n", userState(u))
	if u.QuotaBytes > 0 {
		fmt.Fprintf(tw, "quota\t%s\n", FormatBytes(u.QuotaBytes))
	} else {
		fmt.Fprintf(tw, "quota\tunlimited\n")
	}
	fmt.Fprintf(tw, "expires\t%s\n", expiryText(u))
	if u.Usage != nil {
		fmt.Fprintf(tw, "used (window)\t%s\n", FormatBytes(u.Usage.WindowTotal))
		fmt.Fprintf(tw, "used (lifetime)\t%s\n", FormatBytes(u.Usage.Total))
	}
	if u.SubscriptionURL != "" {
		fmt.Fprintf(tw, "subscription\t%s\n", u.SubscriptionURL)
	}
	fmt.Fprintf(tw, "quota window from\t%s\n", u.UsageResetAt.Format(time.RFC3339))
	if u.Note != "" {
		fmt.Fprintf(tw, "note\t%s\n", u.Note)
	}
	fmt.Fprintf(tw, "created\t%s\n", u.CreatedAt.Format(time.RFC3339))
	tw.Flush()
}

// confirm asks a yes/no question on stderr. A non-interactive stdin answers no, so a
// script that forgets --yes fails safe instead of hanging.
func confirm(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", question)
	var answer string
	if _, err := fmt.Fscanln(cmd.InOrStdin(), &answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
