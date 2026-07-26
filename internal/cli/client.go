package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultSocket is where serve listens for CLI requests.
const DefaultSocket = "/run/vlessvmore/manager.sock"

// SocketEnv overrides DefaultSocket.
const SocketEnv = "VLESSVMORE_SOCKET"

// SocketPath returns the manager socket to talk to.
func SocketPath() string {
	if v := os.Getenv(SocketEnv); v != "" {
		return v
	}
	return DefaultSocket
}

// Client talks to the running daemon over its unix socket.
//
// The socket carries the same HTTP API as the TCP listener, unauthenticated: reaching
// it already requires being root inside the container. That is what makes
// `docker exec vlessvmore vlessvmore user add alice` work with no token setup.
type Client struct {
	socket string
	http   *http.Client
}

// NewClient prepares a client for the socket at path.
func NewClient(path string) *Client {
	return &Client{
		socket: path,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", path)
				},
			},
		},
	}
}

// APIError is a non-2xx response from the daemon.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("server returned %d", e.Status)
	}
	return e.Message
}

// Do performs a request and decodes the JSON response into out, which may be nil.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}

	// The host is ignored by the unix dialer but has to be syntactically present.
	req, err := http.NewRequestWithContext(ctx, method, "http://vlessvmore"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return c.explainDialError(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding response: %w\n%s", err, raw)
	}
	return nil
}

// explainDialError turns "connect: no such file or directory" into something that
// says what is actually wrong: the daemon is not running.
func (c *Client) explainDialError(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) || strings.Contains(err.Error(), c.socket) {
		if _, statErr := os.Stat(c.socket); statErr != nil {
			return fmt.Errorf("no manager listening at %s — is the container running?\n"+
				"  inside the container:  vlessvmore <command>\n"+
				"  from the host:         docker exec vlessvmore vlessvmore <command>", c.socket)
		}
		return fmt.Errorf("cannot reach the manager at %s: %w", c.socket, err)
	}
	return err
}
