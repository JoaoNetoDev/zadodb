package main

import (
	"flag"
	"log"
	"os"

	"github.com/JoaoNetoDev/zadodb/internal/server/config"
	"github.com/JoaoNetoDev/zadodb/internal/server/daemon"
)

// runServe loads configuration (file + flag overrides) and runs the server,
// either in the foreground or as an OS service when launched by the SCM.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to YAML config file")
	dataDir := fs.String("data-dir", "", "data directory (overrides config)")
	httpAddr := fs.String("http-addr", "", "HTTP listen address (overrides config)")
	fsyncMode := fs.String("fsync", "", "fsync mode: per-commit|group-commit (overrides config)")
	if err := fs.Parse(args); err != nil {
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
	if *fsyncMode != "" {
		cfg.Fsync = *fsyncMode
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	logger := log.New(os.Stderr, "zadodb ", log.LstdFlags|log.Lmsgprefix)
	return daemon.Serve(cfg, logger)
}
