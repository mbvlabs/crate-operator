package main

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mbvlabs/crate-operator/internal/api"
	"github.com/mbvlabs/crate-operator/internal/config"
	"github.com/mbvlabs/crate-operator/internal/server"
)

//go:embed api/openapi.yaml
var openapiSpec []byte

var buildVersion = "0.5.2"

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if cfg.APIKey == "" {
		slog.Error("missing API key", "env", "DEPLOY_CRATE_API_KEY")
		os.Exit(1)
	}

	handler := api.NewAPIHandler(buildVersion, cfg.APIKey)
	server.SetOpenAPISpec(openapiSpec)
	srv := server.New(cfg, handler)

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	slog.Info("server stopped")
}
