// Command mock runs the misbehaving fake accounting API.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"sutra/internal/config"
	"sutra/internal/mockacct"
)

func main() {
	config.Load()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg := mockacct.Config{
		FailRate: config.Float("MOCK_FAIL_RATE", 0.20),
		HangRate: config.Float("MOCK_HANG_RATE", 0.10),
		HangFor:  config.Duration("MOCK_HANG_FOR", 5*time.Second),
	}
	addr := config.String("MOCK_ADDR", ":9090")

	log.Info("mock accounting listening", "addr", addr,
		"fail_rate", cfg.FailRate, "hang_rate", cfg.HangRate, "hang_for", cfg.HangFor)
	srv := &http.Server{Addr: addr, Handler: mockacct.New(cfg, log).Routes(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
