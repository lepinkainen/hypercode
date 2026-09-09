// Hypercode is a private, single-user UI for installed coding agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lepinkainen/hypercode/internal/adapter"
	"github.com/lepinkainen/hypercode/internal/adapter/claude"
	"github.com/lepinkainen/hypercode/internal/adapter/codex"
	"github.com/lepinkainen/hypercode/internal/session"
	"github.com/lepinkainen/hypercode/internal/store"
	"github.com/lepinkainen/hypercode/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Hypercode stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	config, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	addr := flag.String("addr", "127.0.0.1:8090", "Loopback or Tailscale IP and port")
	data := flag.String("data-dir", filepath.Join(config, "hypercode"), "Persistent data directory")
	executable := flag.String("codex", "codex", "Codex executable path")
	claudeExecutable := flag.String("claude", "claude", "Claude Code executable path")
	flag.Parse()
	if err = validateAddress(*addr); err != nil {
		return err
	}
	if err = os.MkdirAll(*data, 0700); err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(*data, "hypercode.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	m, err := session.New(db, map[string]adapter.Adapter{"codex": codex.Adapter{Executable: *executable}, "claude": claude.Adapter{Executable: *claudeExecutable}})
	if err != nil {
		return err
	}
	defer func() {
		if err := m.Close(); err != nil {
			slog.Error("save shutdown state", "error", err)
		}
	}()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	app, err := web.New(m, cwd)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	slog.Info("Hypercode is ready", "url", "http://"+listener.Addr().String(), "data", *data, "codex", *executable, "claude", *claudeExecutable)
	select {
	case <-ctx.Done():
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	return server.Shutdown(shutdownCtx)
}
func validateAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errors.New("bind to a loopback or Tailscale IP address")
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return nil
	}
	if netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(ip) {
		return nil
	}
	return fmt.Errorf("public and wildcard listeners are not supported: %s", host)
}
