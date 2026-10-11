// Package api is the HTTP layer: it parses requests, calls the services, and
// turns results and errors into JSON responses. No business rules live here.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sutra/internal/ask"
	"sutra/internal/receipts"
	"sutra/internal/vouchers"
)

type Server struct {
	pool     *pgxpool.Pool
	receipts *receipts.Service
	ask      *ask.Service
	log      *slog.Logger
}

func NewServer(pool *pgxpool.Pool, askSvc *ask.Service, log *slog.Logger) *Server {
	return &Server{pool: pool, receipts: receipts.NewService(pool), ask: askSvc, log: log}
}

// Routes uses Go's built-in router (Go 1.22+ supports "METHOD /path/{param}").
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobwork/receipts", s.createReceipt)
	mux.HandleFunc("GET /issues/{id}", s.getIssue)
	mux.HandleFunc("GET /vouchers", s.listVouchers)
	mux.HandleFunc("POST /vouchers/{id}/retry", s.retryVoucher)
	mux.HandleFunc("POST /ask", s.askQuestion)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return s.logRequests(mux)
}

type createReceiptRequest struct {
	IssueID    int64  `json:"issue_id"`
	Qty        int    `json:"qty"`
	ReceivedOn string `json:"received_on"`
}

func (s *Server) createReceipt(w http.ResponseWriter, r *http.Request) {
	var req createReceiptRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.IssueID <= 0 || req.Qty <= 0 {
		writeError(w, http.StatusBadRequest, "issue_id and qty must be positive")
		return
	}
	if _, err := time.Parse(time.DateOnly, req.ReceivedOn); err != nil {
		writeError(w, http.StatusBadRequest, "received_on must be YYYY-MM-DD")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 200 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key is too long (max 200)")
		return
	}

	// r.Context() is cancelled if the client disconnects; passing it down
	// lets Postgres abandon the work too.
	res, err := s.receipts.Create(r.Context(), receipts.Input{
		IssueID: req.IssueID, Qty: req.Qty, ReceivedOn: req.ReceivedOn, IdempotencyKey: key,
	})
	switch {
	case errors.Is(err, receipts.ErrIssueNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, receipts.ErrOvershoot):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, receipts.ErrKeyMismatch):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		s.serverError(w, r, err)
	case res.Replayed:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, res)
	default:
		writeJSON(w, http.StatusCreated, res)
	}
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sum, err := s.receipts.Summary(r.Context(), id)
	switch {
	case errors.Is(err, receipts.ErrIssueNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		s.serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, sum)
	}
}

func (s *Server) listVouchers(w http.ResponseWriter, r *http.Request) {
	list, err := vouchers.List(r.Context(), s.pool, r.URL.Query().Get("status"))
	switch {
	case errors.Is(err, vouchers.ErrInvalidStatus):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, list)
	}
}

func (s *Server) retryVoucher(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	v, err := vouchers.Retry(r.Context(), s.pool, id)
	switch {
	case errors.Is(err, vouchers.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, vouchers.ErrNotRetryable):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

func (s *Server) askQuestion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil || req.Question == "" {
		writeError(w, http.StatusBadRequest, `body must be {"question": "..."}`)
		return
	}
	ans, err := s.ask.Ask(r.Context(), req.Question)
	if err != nil {
		// Still return the tool calls made so far: they help explain the failure.
		s.log.Error("ask failed", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "tool_calls": ans.ToolCalls})
		return
	}
	writeJSON(w, http.StatusOK, ans)
}

// --- helpers ---

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// serverError logs the real error and returns a generic 500 to the client.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", time.Since(start).Milliseconds())
	})
}
