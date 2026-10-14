package main

import (
	"errors"
	"net"
	"syscall"
	"testing"
)

func TestListenFailureKeepsCause(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	for _, name := range []string{"CURSOR_API_KEY_FILE", "PROXY_API_KEY_FILE", "CURSOR_RPC_BASE_URL", "CURSOR_MODELS_URL", "REQUEST_TIMEOUT", "MAX_CONCURRENT_REQUESTS", "CURSOR_CLIENT_VERSION"} {
		t.Setenv(name, "")
	}
	t.Setenv("CURSOR_API_KEY", "fixture-cursor-key")
	t.Setenv("PROXY_API_KEY", "fixture-proxy-key-0123456789abcdef0123456789")
	t.Setenv("LISTEN_ADDR", occupied.Addr().String())
	if err := run(); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("listen error lost its cause: %v", err)
	}
}
