package singbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultBinary is the sing-box executable, resolved from PATH.
const DefaultBinary = "sing-box"

// BinaryEnv overrides DefaultBinary, mostly so tests and local runs can point at a
// build that is not on PATH.
const BinaryEnv = "VLESSVMORE_SINGBOX_BIN"

// Binary returns the sing-box executable to use.
func Binary() string {
	if v := os.Getenv(BinaryEnv); v != "" {
		return v
	}
	return DefaultBinary
}

// Check validates a config file with `sing-box check`.
//
// This is the gate that makes reloads safe: we never hand sing-box a config we have
// not already proven it accepts, so a bad render leaves the running proxy alone
// instead of taking it down.
func Check(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, Binary(), "check", "-c", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("sing-box rejected the generated config: %s", msg)
	}
	return nil
}

// CheckBytes validates a config document that is not on disk yet.
func CheckBytes(ctx context.Context, dir string, doc []byte) error {
	f, err := os.CreateTemp(dir, "check-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	defer os.Remove(f.Name())

	if _, err := f.Write(doc); err != nil {
		f.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	return Check(ctx, f.Name())
}

// Version reports the sing-box version string, including its build tags. The build
// tags matter: without with_v2ray_api there are no per-user counters at all, so this
// is what proves the image was built correctly.
func Version(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, Binary(), "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s version: %w: %s", Binary(), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// HasV2RayAPI reports whether the sing-box binary was built with with_v2ray_api.
// Without it the stats collector has nothing to talk to.
func HasV2RayAPI(ctx context.Context) (bool, error) {
	v, err := Version(ctx)
	if err != nil {
		return false, err
	}
	return strings.Contains(v, "with_v2ray_api"), nil
}

// writeAtomic writes doc to path via a temp file and a rename, so sing-box can never
// observe a partially written config — including when it is reading concurrently.
func writeAtomic(path string, doc []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp config in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	// The rendered config contains the Reality private key.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if _, err := tmp.Write(doc); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", name, path, err)
	}
	return nil
}
