// Package config loads runtime options without logging credential values.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr     string
	CursorAPIKey   string
	ProxyAPIKey    string
	RPCBaseURL     string
	ModelsURL      string
	RequestTimeout time.Duration
	MaxConcurrent  int
}

func Load() (Config, error) {
	c := Config{ListenAddr: value("LISTEN_ADDR", "127.0.0.1:8787"), RPCBaseURL: value("CURSOR_RPC_BASE_URL", "https://api2.cursor.sh"), ModelsURL: value("CURSOR_MODELS_URL", "https://api.cursor.com/v1/models"), RequestTimeout: 5 * time.Minute, MaxConcurrent: 4}
	var err error
	if c.CursorAPIKey, err = secret("CURSOR_API_KEY"); err != nil {
		return c, err
	}
	if c.ProxyAPIKey, err = secret("PROXY_API_KEY"); err != nil {
		return c, err
	}
	if c.CursorAPIKey == c.ProxyAPIKey {
		return c, fmt.Errorf("PROXY_API_KEY must differ from CURSOR_API_KEY")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return c, fmt.Errorf("LISTEN_ADDR must be host:port")
	}
	if err := validateURL(c.RPCBaseURL); err != nil {
		return c, fmt.Errorf("CURSOR_RPC_BASE_URL: %w", err)
	}
	if err := validateURL(c.ModelsURL); err != nil {
		return c, fmt.Errorf("CURSOR_MODELS_URL: %w", err)
	}
	if s := os.Getenv("REQUEST_TIMEOUT"); s != "" {
		c.RequestTimeout, err = time.ParseDuration(s)
		if err != nil || c.RequestTimeout < time.Second || c.RequestTimeout > 30*time.Minute {
			return c, fmt.Errorf("REQUEST_TIMEOUT must be between 1s and 30m")
		}
	}
	if s := os.Getenv("MAX_CONCURRENT_REQUESTS"); s != "" {
		c.MaxConcurrent, err = strconv.Atoi(s)
		if err != nil || c.MaxConcurrent < 1 || c.MaxConcurrent > 64 {
			return c, fmt.Errorf("MAX_CONCURRENT_REQUESTS must be between 1 and 64")
		}
	}
	return c, nil
}

func value(name, fallback string) string {
	if s := os.Getenv(name); s != "" {
		return s
	}
	return fallback
}

func secret(name string) (string, error) {
	value, path := os.Getenv(name), os.Getenv(name+"_FILE")
	if value != "" && path != "" {
		return "", fmt.Errorf("set only one of %s or %s_FILE", name, name)
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read %s_FILE", name)
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 8192 || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s must contain a nonempty single-line credential", name)
	}
	return value, nil
}

func validateURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("expected an absolute URL without credentials, query or fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme == "http" && (host == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return fmt.Errorf("HTTPS is required except for loopback test servers")
}
