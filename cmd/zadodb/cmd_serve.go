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
	ckManual := fs.Bool("checkpoint-manual", false, "disable auto-checkpoint; fold only via POST /v1/checkpoint (ideal for bulk load)")
	ckMaxOverlay := fs.Int("checkpoint-max-overlay", 0, "safety cap: force a checkpoint when overlay entries exceed this, even in manual mode (0 = off)")
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
	if *ckManual {
		cfg.Checkpoint.Manual = true
	}
	if *ckMaxOverlay > 0 {
		cfg.Checkpoint.MaxOverlay = *ckMaxOverlay
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	logger := log.New(os.Stderr, "zadodb ", log.LstdFlags|log.Lmsgprefix)
	return daemon.Serve(cfg, logger)
}
