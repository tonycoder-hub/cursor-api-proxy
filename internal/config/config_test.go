package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureProxyKey = "fixture-proxy-key-0123456789abcdef0123456789"

// setValidEnv clears every setting and supplies valid credentials.
func setValidEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CURSOR_API_KEY", "CURSOR_API_KEY_FILE", "PROXY_API_KEY", "PROXY_API_KEY_FILE", "LISTEN_ADDR", "CURSOR_RPC_BASE_URL", "CURSOR_MODELS_URL", "REQUEST_TIMEOUT", "MAX_CONCURRENT_REQUESTS", "CURSOR_CLIENT_VERSION"} {
		t.Setenv(name, "")
	}
	t.Setenv("CURSOR_API_KEY", "fixture-cursor-key")
	t.Setenv("PROXY_API_KEY", fixtureProxyKey)
}

func TestCredentialSourcesAndValidation(t *testing.T) {
	setValidEnv(t)
	cfg, err := Load()
	if err != nil || cfg.ListenAddr != "127.0.0.1:8787" {
		t.Fatalf("defaults: %v", err)
	}
	file := filepath.Join(t.TempDir(), "fixture-key")
	if err := os.WriteFile(file, []byte("fixture-file-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURSOR_API_KEY_FILE", file)
	if _, err := Load(); err == nil {
		t.Fatal("ambiguous key sources accepted")
	}
	t.Setenv("CURSOR_API_KEY", "")
	if cfg, err := Load(); err != nil || cfg.CursorAPIKey != "fixture-file-value" {
		t.Fatal("file credential not loaded")
	}
	t.Setenv("CURSOR_API_KEY_FILE", "/does-not-exist/fixture-sensitive-path")
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "fixture-sensitive-path") {
		t.Fatal("file failure missing or sensitive path leaked")
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file failure lost its cause: %v", err)
	}
}

func TestProxyKeyMinimumLength(t *testing.T) {
	setValidEnv(t)
	short := strings.Repeat("k", MinProxyAPIKeyBytes-1)
	t.Setenv("PROXY_API_KEY", short)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "is 31 bytes but must be at least 32 bytes; generate a new key with `openssl rand -hex 32`") || strings.Contains(err.Error(), short) {
		t.Fatalf("short proxy key: %v", err)
	}
	t.Setenv("PROXY_API_KEY", "")
	file := filepath.Join(t.TempDir(), "proxy-key")
	if err := os.WriteFile(file, []byte(short+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_API_KEY_FILE", file)
	if _, err := Load(); err == nil {
		t.Fatal("short proxy key file accepted")
	}
	t.Setenv("PROXY_API_KEY_FILE", "")
	t.Setenv("PROXY_API_KEY", strings.Repeat("k", MinProxyAPIKeyBytes))
	if _, err := Load(); err != nil {
		t.Fatalf("minimum-length proxy key rejected: %v", err)
	}
}

func TestClientVersionOverride(t *testing.T) {
	setValidEnv(t)
	if cfg, err := Load(); err != nil || cfg.ClientVersion != "" {
		t.Fatalf("default client version=%q err=%v", cfg.ClientVersion, err)
	}
	t.Setenv("CURSOR_CLIENT_VERSION", "cli-2026.01.15+build_1")
	if cfg, err := Load(); err != nil || cfg.ClientVersion != "cli-2026.01.15+build_1" {
		t.Fatalf("override client version=%q err=%v", cfg.ClientVersion, err)
	}
	for _, bad := range []string{"cli 2026", "cli\r\nx-injected: 1", "cli/2026", strings.Repeat("a", 65)} {
		t.Setenv("CURSOR_CLIENT_VERSION", bad)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CURSOR_CLIENT_VERSION") {
			t.Errorf("invalid client version %q accepted: %v", bad, err)
		}
	}
}

func TestUpstreamURLValidation(t *testing.T) {
	for _, u := range []string{"https://api2.cursor.sh", "http://127.0.0.1:1234", "http://[::1]:1234", "http://localhost:1234"} {
		if err := validateURL(u); err != nil {
			t.Errorf("valid URL rejected: %s", u)
		}
	}
	for _, u := range []string{"http://example.com", "file:///tmp/file", "https://user:password@example.com", "https://example.com?token=value", "https://example.com#fragment", "//example.com"} {
		if err := validateURL(u); err == nil {
			t.Errorf("invalid URL accepted: %s", u)
		}
	}
}
