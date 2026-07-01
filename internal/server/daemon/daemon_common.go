// Package daemon runs the ZadoDB server in the foreground (supervised by
// systemd on Linux) or as a Windows Service, and manages install/uninstall of
// the OS service.
package daemon

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/server/config"
	httpserver "github.com/JoaoNetoDev/zadodb/internal/server/http"
	"github.com/JoaoNetoDev/zadodb/internal/storage"
)

// ServiceName is the OS service identifier.
const ServiceName = "zadodb"

// ServiceDisplay is the human-readable service name.
const ServiceDisplay = "ZadoDB Database Server"

// RunForeground opens the engine and HTTP server and blocks until ctx is done,
// then shuts down gracefully. It is the shared core of both the interactive and
// service run modes.
func RunForeground(ctx context.Context, cfg config.Config, logger *log.Logger) error {
	eng, err := storage.Open(cfg.StorageConfig())
	if err != nil {
		return err
	}
	defer eng.Close()

	srv := httpserver.New(eng, cfg.HTTPAddr, logger)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		logger.Printf("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Printf("shutdown error: %v", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// serveForeground runs the server until SIGINT/SIGTERM.
func serveForeground(cfg config.Config, logger *log.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return RunForeground(ctx, cfg, logger)
}
