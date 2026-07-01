package http

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/storage"
)

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error envelope.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// errorStatus maps engine sentinel errors to HTTP status codes.
func errorStatus(err error) int {
	switch {
	case errors.Is(err, storage.ErrNoClass), errors.Is(err, storage.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, storage.ErrClassExists), errors.Is(err, storage.ErrClassNotEmpty):
		return http.StatusConflict
	case errors.Is(err, storage.ErrInvalidName):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// writeEngineError maps and writes an engine error.
func writeEngineError(w http.ResponseWriter, err error) {
	writeError(w, errorStatus(err), err.Error())
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// withMiddleware adds panic recovery and request logging.
func withMiddleware(logger *log.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		defer func() {
			if v := recover(); v != nil {
				logger.Printf("panic handling %s %s: %v", r.Method, r.URL.Path, v)
				// Best-effort 500 (headers may already be sent).
				writeError(rec, http.StatusInternalServerError, "internal error")
			}
			logger.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start))
		}()
		h.ServeHTTP(rec, r)
	})
}
