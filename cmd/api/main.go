// Command api serves the Sutra HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sutra/internal/api"
	"sutra/internal/ask"
	"sutra/internal/config"
	"sutra/internal/db"
)

func main() {
	config.Load()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// ctx is cancelled on Ctrl+C or SIGTERM, which starts a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, config.MustString("DATABASE_URL"))
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	askSvc := ask.NewService(ask.Config{
		APIKey:  os.Getenv("OPENROUTER_API_KEY"),
		Model:   config.String("OPENROUTER_MODEL", "anthropic/claude-haiku-5.5"),
		BaseURL: config.String("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
	}, pool, log)
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		log.Warn("OPENROUTER_API_KEY not set: POST /ask uses the stub model")
	}

	srv := &http.Server{
		Addr:              config.String("API_ADDR", ":8080"),
		Handler:           api.NewServer(pool, askSvc, log).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      2 * time.Minute, // /ask may make several LLM calls
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("api listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
