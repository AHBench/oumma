package forms

import (
	"net/http"
	"time"
)

// Health endpoints mirror the Java HealthController + readiness checks.

func (s *Service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "UP",
		"message":     "Application is healthy",
		"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
		"application": "brief-oper-data-index",
		"version":     "1.0.0-SNAPSHOT",
	})
}

func (s *Service) handleHealthLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "UP",
		"check":     "liveness",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *Service) handleHealthReady(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":    "DOWN",
			"check":     "readiness",
			"database":  "disconnected",
			"error":     err.Error(),
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "UP",
		"check":     "readiness",
		"database":  "connected",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *Service) handleHealthStarted(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "UP",
		"check":       "startup",
		"message":     "Application started successfully",
		"application": "brief-oper-data-index",
		"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
	})
}
