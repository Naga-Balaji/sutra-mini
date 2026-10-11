// Command worker pushes pending vouchers to the accounting API.
// Run as many copies as you like; they coordinate through the database.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sutra/internal/config"
	"sutra/internal/db"
	"sutra/internal/syncworker"
)

func main() {
	config.Load()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, config.MustString("DATABASE_URL"))
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	host, _ := os.Hostname()
	w, err := syncworker.New(syncworker.Config{
		// Unique per process, so two workers on one machine never share a lease.
		WorkerID:      config.String("WORKER_ID", fmt.Sprintf("%s-%d", host, os.Getpid())),
		AccountingURL: config.String("ACCOUNTING_URL", "http://localhost:9090"),
		Concurrency:   config.Int("WORKER_CONCURRENCY", 8),
		PollInterval:  config.Duration("WORKER_POLL_INTERVAL", time.Second),
		HTTPTimeout:   config.Duration("WORKER_HTTP_TIMEOUT", 8*time.Second),
		Lease:         config.Duration("WORKER_LEASE", 15*time.Second),
		MaxAttempts:   config.Int("WORKER_MAX_ATTEMPTS", 5),
		BackoffBase:   config.Duration("WORKER_BACKOFF_BASE", 2*time.Second),
		BackoffMax:    config.Duration("WORKER_BACKOFF_MAX", time.Minute),
	}, pool, log)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	if err := w.Run(ctx); err != nil {
		log.Error("worker", "err", err)
		os.Exit(1)
	}
}
