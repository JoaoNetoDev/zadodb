// Package http exposes the storage engine over a small REST/JSON API using only
// the standard library's net/http (Go 1.22 method-aware routing, no framework).
package http

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/storage"
)

// Server wires the storage engine to an HTTP listener.
type Server struct {
	engine *storage.Engine
	http   *http.Server
	logger *log.Logger
}

// New builds a server bound to addr over the given engine.
func New(engine *storage.Engine, addr string, logger *log.Logger) *Server {
	s := &Server{engine: engine, logger: logger}
	s.http = &http.Server{
		Addr:              addr,
		Handler:           withMiddleware(logger, s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// routes registers all endpoints.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/stats", s.handleStats)

	mux.HandleFunc("POST /v1/classes", s.handleCreateClass)
	mux.HandleFunc("GET /v1/classes", s.handleListClasses)
	mux.HandleFunc("GET /v1/classes/{class}", s.handleGetClass)
	mux.HandleFunc("DELETE /v1/classes/{class}", s.handleDeleteClass)

	mux.HandleFunc("POST /v1/classes/{class}/objects", s.handleCreateObject)
	mux.HandleFunc("GET /v1/classes/{class}/objects", s.handleListObjects)
	mux.HandleFunc("GET /v1/classes/{class}/objects/{id}", s.handleGetObject)
	mux.HandleFunc("PUT /v1/classes/{class}/objects/{id}", s.handlePutObject)
	mux.HandleFunc("DELETE /v1/classes/{class}/objects/{id}", s.handleDeleteObject)

	return mux
}

// Handler returns the fully-wrapped handler (used by tests).
func (s *Server) Handler() http.Handler { return s.http.Handler }

// ListenAndServe starts serving and blocks until the server is shut down.
func (s *Server) ListenAndServe() error {
	s.logger.Printf("listening on %s", s.http.Addr)
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.engine.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"last_tx_id":      st.LastTxID,
		"wal_bytes":       st.WALBytes,
		"active_gen":      st.ActiveGen,
		"num_classes":     st.NumClasses,
		"overlay_size":    st.OverlaySize,
		"checkpoints":     st.Checkpoints,
		"last_checkpoint": st.LastCheckpnt,
	})
}
