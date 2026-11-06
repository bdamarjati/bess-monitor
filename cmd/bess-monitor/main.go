// Command bess-monitor subscribes to BESS telemetry over MQTT, stores it in SQLite,
// and serves the HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bess-monitor/internal/api"
	"bess-monitor/internal/auth"
	"bess-monitor/internal/config"
	"bess-monitor/internal/ingest"
	"bess-monitor/internal/store"
	"bess-monitor/web"
)

const flushEvery = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	login, err := auth.New(cfg.DashUser, cfg.DashPass, cfg.SessionSecret)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The batcher has its own context so it can do a final flush
	// after the subscriber has disconnected.
	batcher := ingest.NewBatcher(flushEvery, db.Insert)
	bctx, stopBatcher := context.WithCancel(context.Background())
	batcherDone := make(chan struct{})
	go func() {
		batcher.Run(bctx)
		close(batcherDone)
	}()

	// Minute/hour rollups and daily cleanup.
	retention := store.Retention{
		Raw:    days(cfg.RetentionRawDays),
		Minute: days(cfg.RetentionMinDays),
		Hour:   days(cfg.RetentionHrDays),
	}
	maintDone := make(chan struct{})
	go func() {
		db.Maintain(ctx, retention)
		close(maintDone)
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           routes(db, login),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	srvErr := make(chan error, 1)
	go func() {
		slog.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("http: %w", err)
			stop() // shut everything down
		}
	}()

	host, _ := os.Hostname()
	opts := ingest.Options{
		URL:      cfg.MQTTURL,
		User:     cfg.MQTTUser,
		Pass:     cfg.MQTTPass,
		Topic:    cfg.MQTTTopic,
		ClientID: "bess-monitor-" + host,
	}
	slog.Info("starting", "broker", cfg.MQTTURL, "topic", cfg.MQTTTopic, "db", cfg.DBPath,
		"retention_days", fmt.Sprintf("raw=%d 1m=%d 1h=%d", cfg.RetentionRawDays, cfg.RetentionMinDays, cfg.RetentionHrDays))
	err = ingest.Run(ctx, opts, batcher.Add) // blocks until shutdown

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := srv.Shutdown(shutCtx); e != nil {
		slog.Warn("http shutdown", "err", e)
	}
	stopBatcher()
	<-batcherDone
	<-maintDone
	slog.Info("stopped")

	select {
	case e := <-srvErr:
		return e
	default:
		return err
	}
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// routes puts the sign-in handlers in front of the API and dashboard.
func routes(db *store.Store, login *auth.Auth) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", login.Middleware(api.New(db, web.Files, login.LoginPage)))
	mux.HandleFunc("POST /login", login.Login)
	mux.HandleFunc("POST /logout", login.Logout)
	return mux
}
