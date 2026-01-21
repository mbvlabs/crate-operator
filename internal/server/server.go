// Package server implements the HTTP server for the Mithlond Agent.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/mbvlabs/mithlond-agent/internal/api"
	"github.com/mbvlabs/mithlond-agent/internal/config"
)

type Server struct {
	cfg        *config.Config
	httpServer *http.Server
	handler    api.ServerInterface
}

func New(cfg *config.Config, handler api.ServerInterface) *Server {
	return &Server{
		cfg:     cfg,
		handler: handler,
	}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Register OpenAPI-generated routes
	apiHandler := api.HandlerFromMux(s.handler, mux)

	// Register additional routes (docs)
	mux.HandleFunc("GET /docs", s.handleDocs)
	mux.HandleFunc("GET /openapi.yaml", s.handleOpenAPISpec)

	// Wrap with middleware
	var h http.Handler = apiHandler
	h = s.requestLoggingMiddleware(h)
	h = s.requestIDMiddleware(h)

	s.httpServer = &http.Server{
		Addr:         s.cfg.BindAddr,
		Handler:      h,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	slog.Info("starting server", "addr", s.cfg.BindAddr)
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = generateRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requestLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration", time.Since(start),
			"request_id", r.Context().Value(requestIDKey),
		)
	})
}

type contextKey string

const requestIDKey contextKey = "request_id"

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
