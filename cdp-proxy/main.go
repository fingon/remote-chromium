package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
)

type cli struct {
	Listen   string `help:"Listen address; a bare port becomes ':<port>'." env:"CDP_PORT" default:"9222"`
	Upstream string `help:"Upstream chromium DevTools host:port; a bare port becomes '127.0.0.1:<port>'." env:"CDP_INTERNAL_PORT" default:"9223"`
	Verbose  bool   `help:"Enable debug logging." short:"v"`
}

// normalizeListen accepts "9222", ":9222", or "host:port".
func normalizeListen(addr string) (string, error) {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr, nil
	}
	if port, err := parsePort(addr); err == nil {
		return ":" + port, nil
	}
	return "", fmt.Errorf("invalid listen address %q", addr)
}

// normalizeUpstream accepts "9223" (loopback implied) or "host:port".
func normalizeUpstream(addr string) (string, error) {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr, nil
	}
	if port, err := parsePort(addr); err == nil {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	return "", fmt.Errorf("invalid upstream address %q", addr)
}

func parsePort(s string) (string, error) {
	p, err := net.LookupPort("tcp", s)
	if err != nil || p <= 0 {
		return "", fmt.Errorf("%q is not a TCP port", s)
	}
	return s, nil
}

func run(cfg cli) error {
	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	listenAddr, err := normalizeListen(cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	upstreamAddr, err := normalizeUpstream(cfg.Upstream)
	if err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	advertised := advertisedHost(listenAddr)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           newProxyHandler(upstreamAddr, advertised),
		ReadHeaderTimeout: 5 * time.Second,
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	slog.Info("cdp-proxy listening",
		"addr", listenAddr,
		"upstream_addr", upstreamAddr,
		"advertised_host", advertised)

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	case <-rootCtx.Done():
		slog.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func main() {
	var cfg cli
	kong.Parse(&cfg,
		kong.Name("cdp-proxy"),
		kong.Description("Reverse proxy making chromium's loopback-only DevTools endpoint addressable by hostname."),
		kong.UsageOnError(),
	)
	if err := run(cfg); err != nil {
		slog.Error("cdp-proxy failed", "err", err)
		os.Exit(1)
	}
}
