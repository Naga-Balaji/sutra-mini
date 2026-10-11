// Package mockacct is a fake accounting API that misbehaves on purpose:
//   - FailRate of calls return HTTP 500 (and record nothing)
//   - HangRate of calls sleep HangFor, then succeed
//   - a client_ref it has seen before returns HTTP 409 with the original external_ref
//
// It keeps its records in memory, so restarting the mock forgets them.
package mockacct

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

type Config struct {
	FailRate float64       // 0.20 in the brief
	HangRate float64       // 0.10 in the brief
	HangFor  time.Duration // 5s in the brief
}

type Server struct {
	cfg Config
	log *slog.Logger

	mu   sync.Mutex        // protects the two fields below
	refs map[string]string // client_ref -> external_ref
	next int
}

func New(cfg Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log, refs: map[string]string{}}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vouchers", s.postVoucher)
	return mux
}

type voucherRequest struct {
	ClientRef   string `json:"client_ref"`
	Type        string `json:"type"`
	SourceID    int64  `json:"source_id"`
	AmountPaise int64  `json:"amount_paise"`
}

func (s *Server) postVoucher(w http.ResponseWriter, r *http.Request) {
	var req voucherRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClientRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client_ref is required"})
		return
	}

	roll := rand.Float64()
	switch {
	case roll < s.cfg.FailRate:
		s.log.Warn("mock: random 500", "client_ref", req.ClientRef)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "random failure"})
		return
	case roll < s.cfg.FailRate+s.cfg.HangRate:
		// Deliberately NOT cancelled when the client gives up: like a real
		// slow server, the voucher is still recorded after the hang.
		s.log.Warn("mock: hanging", "client_ref", req.ClientRef, "for", s.cfg.HangFor)
		time.Sleep(s.cfg.HangFor)
	}

	s.mu.Lock()
	ext, seen := s.refs[req.ClientRef]
	if !seen {
		s.next++
		ext = fmt.Sprintf("ACC-%06d", s.next)
		s.refs[req.ClientRef] = ext
	}
	s.mu.Unlock()

	if seen {
		s.log.Info("mock: duplicate", "client_ref", req.ClientRef, "external_ref", ext)
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "duplicate client_ref", "external_ref": ext,
		})
		return
	}
	s.log.Info("mock: created", "client_ref", req.ClientRef, "external_ref", ext,
		"amount_paise", req.AmountPaise)
	writeJSON(w, http.StatusCreated, map[string]string{"external_ref": ext})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
