package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func withClient(ctx context.Context, c *Client) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, clientKey{}, c)
}

// client returns the socket client for this invocation.
func client(cmd *cobra.Command) *Client {
	if c, ok := cmd.Context().Value(clientKey{}).(*Client); ok {
		return c
	}
	// Reached only if a command runs without PersistentPreRun, e.g. from a test.
	return NewClient(SocketPath())
}
