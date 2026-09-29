package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialSourcesAndValidation(t *testing.T) {
	for _, name := range []string{"CURSOR_API_KEY", "CURSOR_API_KEY_FILE", "PROXY_API_KEY", "PROXY_API_KEY_FILE", "LISTEN_ADDR", "CURSOR_RPC_BASE_URL", "CURSOR_MODELS_URL", "REQUEST_TIMEOUT", "MAX_CONCURRENT_REQUESTS"} {
		t.Setenv(name, "")
	}
	t.Setenv("CURSOR_API_KEY", "fixture-cursor-key")
	t.Setenv("PROXY_API_KEY", "fixture-proxy-key")
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
