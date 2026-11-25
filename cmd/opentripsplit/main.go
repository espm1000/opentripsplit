// Command opentripsplit runs the OpenTripSplit web server.
//
//	opentripsplit             start the server (configured from env vars)
//	opentripsplit healthcheck exit 0 if the local server answers /healthz
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/espm1000/opentripsplit"
	"github.com/espm1000/opentripsplit/internal/auth"
	"github.com/espm1000/opentripsplit/internal/config"
	"github.com/espm1000/opentripsplit/internal/store"
	"github.com/espm1000/opentripsplit/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	// The level is raised to debug once config is loaded, if DEBUG is on.
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(log, level); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.Debug {
		level.Set(slog.LevelDebug)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	a := &auth.Auth{Store: st, SessionTTL: cfg.SessionTTL, Secure: cfg.Secure(), Log: log}
	// Providers come from env vars and/or the database (set up at /setup).
	srv, err := web.New(ctx, st, a, web.Options{
		BaseURL: cfg.BaseURL, InviteTTL: cfg.InviteTTL, EnvProviders: cfg.EnvProviders(),
		DevLocalAuth: cfg.DevLocalAuth, Simulator: cfg.Simulator, Version: opentripsplit.Version(), Log: log,
	})
	if err != nil {
		return err
	}

	go purgeSessions(ctx, st, log)

	hs := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "version", opentripsplit.Version(), "addr", cfg.ListenAddr, "tls", cfg.TLS(), "base_url", cfg.BaseURL.String(), "db", cfg.DBPath)
		if cfg.TLS() {
			errc <- hs.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// purgeSessions deletes expired sessions at startup and then hourly.
func purgeSessions(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.PurgeExpiredSessions(ctx); err != nil {
			log.Error("purge sessions", "err", err)
		} else if n > 0 {
			log.Info("purged expired sessions", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// healthcheck is for container HEALTHCHECK: distroless images have no shell
// or curl, so the binary checks itself. It uses HTTPS when TLS_CERT_FILE is
// set, like the server.
//
// #nosec G704 -- not SSRF: the URL is fixed loopback, only the port comes from LISTEN_ADDR.
func healthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad LISTEN_ADDR:", err)
		return 1
	}
	scheme := "http"
	c := &http.Client{Timeout: 3 * time.Second}
	if strings.TrimSpace(os.Getenv("TLS_CERT_FILE")) != "" {
		scheme = "https"
		// The certificate names the public host, not 127.0.0.1, and this only
		// checks that our own process answers on loopback.
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- loopback self-check, see above
	}
	resp, err := c.Get(scheme + "://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}
