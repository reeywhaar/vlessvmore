// Command backup fetches a vlessvmore backup archive and forwards it to backio.
//
// One shot per invocation: fetch, optionally encrypt, upload, prune. The schedule lives
// in backup_loop.sh, so a stuck run cannot skip the next one silently — the container
// restarts instead.
//
// A close sibling of github.com/Reeywhaar/vaultwarden_backup, deliberately: same
// environment variables, same log format, same retention policy, so the two behave alike
// wherever they run side by side.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// defaultBackupDir is where archives are kept locally. Mount a volume over it to keep
// copies that do not depend on the remote being reachable.
const defaultBackupDir = "/backups"

// fetchTimeout bounds the download. Generous, because the server builds the archive
// before answering and a large usage history takes a moment.
const fetchTimeout = 5 * time.Minute

type config struct {
	dir          string
	source       string
	url          string
	provider     string
	subdirectory string
	token        string
	password     string
}

func main() {
	if err := backup(); err != nil {
		logError("backup", err.Error())
		os.Exit(1)
	}
}

func backup() error {
	cfg := config{
		dir:          envOr("BACKUP_DIR", defaultBackupDir),
		source:       envOr("VLESSVMORE_URL", "http://vlessvmore:3000"),
		url:          envOr("BACKIO_URL", "http://backio:8080"),
		provider:     envOr("BACKIO_PROVIDER", "gdrive"),
		subdirectory: os.Getenv("BACKIO_SUBDIRECTORY"),
		token:        os.Getenv("BACKUP_TOKEN"),
		password:     os.Getenv("BACKUP_PASSWORD"),
	}
	if cfg.subdirectory == "" {
		return fmt.Errorf("BACKIO_SUBDIRECTORY is not set")
	}

	log("backup", "Starting backup")

	if err := os.MkdirAll(cfg.dir, 0o700); err != nil {
		return err
	}

	ts := timestamp()
	archiveName := fmt.Sprintf("vlessvmore-%s.tgz", ts)
	archive := filepath.Join(cfg.dir, archiveName)

	if err := fetch(cfg.source, archive); err != nil {
		return err
	}
	log("fetch", "Fetched "+archiveName)

	if cfg.password != "" {
		encryptedName := fmt.Sprintf("vlessvmore-%s.zip", ts)
		encrypted := filepath.Join(cfg.dir, encryptedName)

		log("encrypt", "Encrypting to "+encryptedName)
		// -mx=1: the payload is already gzipped, so compressing again buys nothing.
		if err := run("7z", "a", "-tzip", "-p"+cfg.password, "-mem=AES256", "-mx=1",
			encrypted, archive); err != nil {
			return err
		}
		// 7z creates the file at the umask, unlike the download, which opens it 0600.
		// Encrypted or not, the contents are the Reality private key.
		if err := os.Chmod(encrypted, 0o600); err != nil {
			return err
		}
		if err := os.Remove(archive); err != nil {
			return err
		}
		archive, archiveName = encrypted, encryptedName
	}

	if err := uploadToBackio(archive, archiveName, cfg); err != nil {
		return err
	}

	cleanupLocalBackups(cfg)
	cleanupRemoteBackups(cfg)
	return nil
}

// fetch downloads the archive to a .part file and renames it on success, so an
// interrupted download never enters the retention pool as a valid-looking backup.
func fetch(source, dest string) error {
	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Get(source + "/backup")
	if err != nil {
		return fmt.Errorf("fetch %s: %w", source, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("fetch %s: %d: %s", source, resp.StatusCode, string(body))
	}

	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		f.Close()
		os.Remove(part)
		return fmt.Errorf("write %s: %w", part, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	// The server sends a Content-Length, so a short read is detectable rather than
	// something to discover at restore time.
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		os.Remove(part)
		return fmt.Errorf("fetch %s: got %d bytes, Content-Length said %d", source, n, resp.ContentLength)
	}
	return os.Rename(part, dest)
}

func cleanupLocalBackups(cfg config) {
	log("cleanup", "Running local retention policy: 3 daily, 1 weekly, 1 monthly")

	names, err := listLocalBackups(cfg.dir)
	if err != nil {
		logError("cleanup", "Failed to list local backups: "+err.Error())
		return
	}
	for _, name := range toRemove(names, time.Now()) {
		log("cleanup", "Removing local: "+name)
		if err := os.Remove(filepath.Join(cfg.dir, name)); err != nil {
			logError("cleanup", fmt.Sprintf("Failed to remove local %s: %s", name, err))
		}
	}
}

func cleanupRemoteBackups(cfg config) {
	if cfg.token == "" {
		return
	}
	log("remote", "Running remote retention policy: 3 daily, 1 weekly, 1 monthly")

	names, err := listBackioBackups(cfg)
	if err != nil {
		logError("remote", "Failed to list remote backups: "+err.Error())
		return
	}
	for _, name := range toRemove(names, time.Now()) {
		log("remote", "Removing remote: "+name)
		if err := deleteBackioBackup(name, cfg); err != nil {
			logError("remote", fmt.Sprintf("Failed to delete remote %s: %s", name, err))
		}
	}
}

func uploadToBackio(archive, archiveName string, cfg config) error {
	if cfg.token == "" {
		log("remote", "Skipping upload: BACKUP_TOKEN not set")
		return nil
	}

	log("remote", "Uploading "+archiveName+" to "+cfg.provider)

	fileBytes, err := os.ReadFile(archive)
	if err != nil {
		return err
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("backup", archiveName)
	if err != nil {
		return err
	}
	if _, err := part.Write(fileBytes); err != nil {
		return err
	}
	for k, v := range map[string]string{
		"name":         archiveName,
		"subdirectory": cfg.subdirectory,
		"provider":     cfg.provider,
	} {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, cfg.url+"/backup", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	responseBytes, _ := io.ReadAll(resp.Body)
	responseText := string(responseBytes)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload failed (%d): %s", resp.StatusCode, responseText)
	}

	var result struct {
		Status      string `json:"status"`
		Destination string `json:"destination"`
	}
	if err := json.Unmarshal(responseBytes, &result); err != nil {
		return fmt.Errorf("invalid response: %s", responseText)
	}
	if result.Status != "ok" {
		return fmt.Errorf("upload failed: %s", responseText)
	}

	log("remote", "Remote backup success: "+result.Destination)
	return nil
}

func listBackioBackups(cfg config) ([]string, error) {
	params := url.Values{}
	params.Set("provider", cfg.provider)
	params.Set("subdirectory", cfg.subdirectory)

	req, err := http.NewRequest(http.MethodGet, cfg.url+"/backup?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// A create-only token is a legitimate configuration: it produces working backups
	// with no remote pruning, rather than an error every hour.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log("remote", "Skipping remote list: insufficient permissions")
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list failed (%d): %s", resp.StatusCode, string(text))
	}

	var items []struct {
		Name  string `json:"Name"`
		IsDir bool   `json:"IsDir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}

	var names []string
	for _, item := range items {
		if !item.IsDir {
			names = append(names, item.Name)
		}
	}
	return names, nil
}

func deleteBackioBackup(name string, cfg config) error {
	params := url.Values{}
	params.Set("provider", cfg.provider)
	params.Set("subdirectory", cfg.subdirectory)
	params.Set("name", name)

	req, err := http.NewRequest(http.MethodDelete, cfg.url+"/backup?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		logError("remote", "Skipping remote delete "+name+": insufficient permissions")
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("delete failed (%d): %s", resp.StatusCode, string(text))
	}
	return nil
}

func listLocalBackups(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func log(operation, message string) {
	logMsg("info", operation, message)
}

func logError(operation, message string) {
	logMsg("error", operation, message)
}

func logMsg(level, operation, message string) {
	entry := map[string]string{
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		"level":     level,
		"operation": operation,
		"message":   message,
	}
	b, _ := json.Marshal(entry)
	if level == "error" {
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		fmt.Fprintln(os.Stdout, string(b))
	}
}

// timestamp is UTC, matching the format retention.go parses back out.
func timestamp() string {
	return time.Now().UTC().Format("20060102_150405")
}

func run(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s failed: %s", cmd, msg)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
