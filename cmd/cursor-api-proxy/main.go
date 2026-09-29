package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tonycoder-hub/cursor-api-proxy/internal/config"
	"github.com/tonycoder-hub/cursor-api-proxy/internal/proxy"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument; configure the service through environment variables")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	handler, err := proxy.New(proxy.NewCursorBackend(cfg), cfg.ProxyAPIKey, cfg.RequestTimeout, cfg.MaxConcurrent)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("cannot listen on configured LISTEN_ADDR")
	}
	log.Printf("cursor-api-proxy %s listening on %s", version, listener.Addr())
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP server stopped unexpectedly")
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("HTTP shutdown timed out")
		}
		return nil
	}
}
