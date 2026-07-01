package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/JoaoNetoDev/zadodb/internal/server/config"
	"github.com/JoaoNetoDev/zadodb/internal/server/daemon"
)

// runService manages the OS-level service: install, uninstall, start, stop, status.
func runService(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: zadodb service <install|uninstall|start|stop|status> [flags]")
	}
	action := args[0]

	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to YAML config file")
	dataDir := fs.String("data-dir", "", "data directory")
	httpAddr := fs.String("http-addr", "", "HTTP listen address")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}
	// Services run from a system working directory, so store an absolute path.
	if abs, err := filepath.Abs(cfg.DataDir); err == nil {
		cfg.DataDir = abs
	}

	switch action {
	case "install":
		return daemon.Install(cfg)
	case "uninstall":
		return daemon.Uninstall()
	case "start":
		return daemon.Start()
	case "stop":
		return daemon.Stop()
	case "status":
		return daemon.Status()
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
}
