// Package config loads ZadoDB server configuration from a YAML file, layered
// over safe defaults, and translates it into the storage engine's config.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/storage"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
	"gopkg.in/yaml.v3"
)

// Config is the server configuration.
type Config struct {
	DataDir  string `yaml:"data_dir"`
	HTTPAddr string `yaml:"http_addr"`

	// Fsync selects durability: "per-commit" (default, safest) or
	// "group-commit" (higher throughput, tiny durability window).
	Fsync string `yaml:"fsync"`

	GroupCommit struct {
		IntervalMs int `yaml:"interval_ms"`
		MaxBatch   int `yaml:"max_batch"`
	} `yaml:"group_commit"`

	Checkpoint struct {
		WALBytes    int64 `yaml:"wal_bytes"`
		IntervalSec int   `yaml:"interval_sec"`
		Manual      bool  `yaml:"manual"`      // disable auto-checkpoint; fold only via POST /v1/checkpoint
		MaxOverlay  int   `yaml:"max_overlay"` // safety cap: force a fold when overlay exceeds this (even in manual mode)
	} `yaml:"checkpoint"`
}

// Default returns the built-in defaults.
func Default() Config {
	c := Config{
		DataDir:  "./data",
		HTTPAddr: "127.0.0.1:7373",
		Fsync:    "per-commit",
	}
	c.GroupCommit.IntervalMs = 2
	c.GroupCommit.MaxBatch = 256
	c.Checkpoint.WALBytes = 64 << 20
	c.Checkpoint.IntervalSec = 300
	return c
}

// Load reads a YAML config file layered over defaults. A missing path returns
// the defaults unchanged.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate checks the configuration for obvious problems.
func (c Config) Validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("config: data_dir must be set")
	}
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: http_addr must be set")
	}
	switch c.Fsync {
	case "per-commit", "group-commit":
	default:
		return fmt.Errorf("config: fsync must be per-commit or group-commit, got %q", c.Fsync)
	}
	return nil
}

// FsyncMode translates the fsync setting into the WAL's mode.
func (c Config) FsyncMode() wal.FsyncMode {
	if c.Fsync == "group-commit" {
		return wal.GroupCommitMode(
			time.Duration(c.GroupCommit.IntervalMs)*time.Millisecond,
			c.GroupCommit.MaxBatch,
		)
	}
	return wal.DefaultFsyncMode()
}

// StorageConfig builds the engine configuration.
func (c Config) StorageConfig() storage.Config {
	return storage.Config{
		Dir:                  c.DataDir,
		Fsync:                c.FsyncMode(),
		CheckpointWALBytes:   c.Checkpoint.WALBytes,
		CheckpointInterval:   time.Duration(c.Checkpoint.IntervalSec) * time.Second,
		CheckpointManual:     c.Checkpoint.Manual,
		CheckpointMaxOverlay: c.Checkpoint.MaxOverlay,
	}
}
