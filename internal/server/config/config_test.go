package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

func TestDefaultLoads(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if c.DataDir != "./data" || c.HTTPAddr != "127.0.0.1:7373" || c.Fsync != "per-commit" {
		t.Fatalf("defaults = %+v", c)
	}
	if c.FsyncMode().Policy != wal.FsyncPerCommit {
		t.Fatalf("default fsync policy not per-commit")
	}
}

func TestLoadOverlaysFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zadodb.yaml")
	yaml := `
data_dir: /var/lib/zadodb
http_addr: 0.0.0.0:9000
fsync: group-commit
group_commit:
  interval_ms: 5
  max_batch: 512
checkpoint:
  wal_bytes: 1048576
  interval_sec: 60
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DataDir != "/var/lib/zadodb" || c.HTTPAddr != "0.0.0.0:9000" {
		t.Fatalf("file not applied: %+v", c)
	}
	if c.FsyncMode().Policy != wal.FsyncGroupCommit {
		t.Fatalf("fsync policy not group-commit")
	}
	sc := c.StorageConfig()
	if sc.CheckpointWALBytes != 1048576 || sc.Dir != "/var/lib/zadodb" {
		t.Fatalf("storage config = %+v", sc)
	}
}

func TestMissingFileReturnsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if c.DataDir != "./data" {
		t.Fatalf("missing file should give defaults, got %+v", c)
	}
}

func TestValidateRejectsBadFsync(t *testing.T) {
	c := Default()
	c.Fsync = "sometimes"
	if err := c.Validate(); err == nil {
		t.Fatal("expected validation error for bad fsync")
	}
}
