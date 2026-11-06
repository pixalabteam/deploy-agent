package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestReplaceBinaryVerifiesChecksum(t *testing.T) {
	binary := []byte("new-binary")
	sum := sha256.Sum256(binary)
	goodSum := hex.EncodeToString(sum[:]) + "  deploy-agent\n"

	serve := func(checksum string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			if strings.HasSuffix(r.URL.Path, ".sha256") {
				_, _ = w.Write([]byte(checksum))
				return
			}
			_, _ = w.Write(binary)
		}))
	}

	t.Run("good checksum replaces the binary", func(t *testing.T) {
		ts := serve(goodSum)
		defer ts.Close()
		exe := t.TempDir() + "/deploy-agent"
		_ = os.WriteFile(exe, []byte("old"), 0o755)
		cfg := Config{AgentUpdateURL: ts.URL + "/deploy-agent-linux-{arch}", AgentUpdateToken: "tok"}
		if err := cfg.replaceBinary(t.Context(), exe); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(exe); string(got) != "new-binary" {
			t.Errorf("binary = %q", got)
		}
	})

	t.Run("bad checksum keeps the old binary", func(t *testing.T) {
		ts := serve(strings.Repeat("0", 64))
		defer ts.Close()
		exe := t.TempDir() + "/deploy-agent"
		_ = os.WriteFile(exe, []byte("old"), 0o755)
		cfg := Config{AgentUpdateURL: ts.URL + "/x", AgentUpdateToken: "tok"}
		if err := cfg.replaceBinary(t.Context(), exe); err == nil {
			t.Fatal("expected checksum mismatch")
		}
		if got, _ := os.ReadFile(exe); string(got) != "old" {
			t.Errorf("binary was replaced despite bad checksum: %q", got)
		}
	})
}
