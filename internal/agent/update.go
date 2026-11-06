package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// updateExitCode is non-zero on purpose: the systemd unit uses Restart=on-failure,
// so a clean exit(0) would leave the agent stopped after a self-update.
const updateExitCode = 75

const maxBinaryBytes = 100 << 20

func (c Config) updateURL() string {
	return strings.ReplaceAll(c.AgentUpdateURL, "{arch}", runtime.GOARCH)
}

// SelfUpdate downloads the binary at AGENT_UPDATE_URL (plus "<url>.sha256"),
// verifies the checksum, and atomically replaces the running executable. It does
// not restart the process; the caller exits afterwards so systemd starts the new one.
func (c Config) SelfUpdate(ctx context.Context) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}
	return c.replaceBinary(ctx, exe)
}

func (c Config) replaceBinary(ctx context.Context, exe string) error {
	if c.AgentUpdateURL == "" {
		return fmt.Errorf("AGENT_UPDATE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	url := c.updateURL()
	sumBody, err := c.download(ctx, url+".sha256", 1<<10)
	if err != nil {
		return fmt.Errorf("download checksum: %w", err)
	}
	fields := strings.Fields(string(sumBody))
	if len(fields) == 0 {
		return fmt.Errorf("checksum file is empty")
	}
	want := strings.ToLower(fields[0])

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".deploy-agent-update-*")
	if err != nil {
		return fmt.Errorf("create temp file next to binary (is the directory writable by the agent user?): %w", err)
	}
	defer os.Remove(tmp.Name())

	hash := sha256.New()
	if err := c.downloadTo(ctx, url, io.MultiWriter(tmp, hash)); err != nil {
		tmp.Close()
		return fmt.Errorf("download binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}

func (c Config) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	var buf strings.Builder
	if err := c.downloadTo(ctx, url, &limitedWriter{w: &buf, left: limit}); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func (c Config) downloadTo(ctx context.Context, url string, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if c.AgentUpdateToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AgentUpdateToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	_, err = io.Copy(dst, io.LimitReader(resp.Body, maxBinaryBytes))
	return err
}

type limitedWriter struct {
	w    io.Writer
	left int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, fmt.Errorf("response larger than %d bytes", l.left)
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}
