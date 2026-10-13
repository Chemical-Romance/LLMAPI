package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-api/internal/api"
	"llm-api/internal/config"
	"llm-api/internal/db"
	"llm-api/internal/route"
	"llm-api/internal/store"
	"llm-api/internal/web"
)

func main() {
	cfg := config.Get()
	log.Printf("[init] config: %s", cfg.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := db.Connect(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("[init] mongo connect failed: %v", err)
	}
	defer func() { _ = d.Client.Disconnect(context.Background()) }()
	log.Printf("[init] mongo connected")

	// seed admin + shared auth secret (first run only)
	adminPass, err := api.EnsureAdmin(ctx, d, cfg)
	if err != nil {
		log.Fatalf("[init] seed admin failed: %v", err)
	}
	secret, err := api.EnsureAuthSecret(ctx, d)
	if err != nil {
		log.Fatalf("[init] seed auth secret failed: %v", err)
	}
	if adminPass != "" {
		log.Printf("[init] admin user %q created; password: %s  (save it now, shown once)", cfg.AdminUser, adminPass)
	}

	// wiring
	st := store.New(d)
	engine := route.NewEngine(st, cfg.FailoverCooldown())
	gw := api.NewGateway(cfg, st, engine)
	adminAuth := api.NewAdminAuth(secret)
	adm := api.NewAdmin(st, adminAuth, gw)

	mux := http.NewServeMux()
	adm.Register(mux)                      // /api/admin/*
	gw.Register(mux)                       // /v1/*
	mux.Handle("/console/", web.Handler()) // admin console (embedded SPA)
	mux.Handle("/", http.RedirectHandler("/console/", http.StatusMovedPermanently))

	// No WriteTimeout: SSE responses must be allowed to stream indefinitely.
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() {
		// tcp4: Go's ":port" would bind an IPv6 dual-stack socket, and the
		// WSL->Windows localhost relay then only exposes [::1], leaving
		// 127.0.0.1 unreachable from Windows.
		l, err := net.Listen("tcp4", cfg.HTTPAddr)
		if err != nil {
			errCh <- err
			return
		}
		log.Printf("[serve] listening on http://%s  (console: http://%s/console/, admin API: /api/admin/*)", cfg.HTTPAddr, cfg.HTTPAddr)
		errCh <- srv.Serve(l)
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[serve] %v", err)
		}
	case <-ctx.Done():
		log.Printf("[shutdown] draining…")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		log.Printf("[shutdown] bye")
	}
}
