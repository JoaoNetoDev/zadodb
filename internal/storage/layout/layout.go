// Package layout defines ZadoDB's on-disk file layout and the crash-safe
// "CURRENT" pointer that names the active data-file generation.
//
// Data files are versioned by generation (data.NNNNNN.zdb) rather than being
// renamed in place. A tiny CURRENT file names the active generation and is
// updated atomically (write temp + rename). This sidesteps the Windows
// restriction that a memory-mapped file cannot be replaced: the live data file
// is never renamed over — a new generation is a new filename, and only the
// small, never-held-open CURRENT pointer is rename-replaced.
package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	dataPrefix  = "data."
	dataSuffix  = ".zdb"
	currentTmp  = "CURRENT.tmp"
	currentName = "CURRENT"
	walName     = "wal.log"
)

// DataFile returns the path of the data file for a generation.
func DataFile(dir string, gen uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%s%06d%s", dataPrefix, gen, dataSuffix))
}

// CurrentFile returns the path of the CURRENT pointer file.
func CurrentFile(dir string) string { return filepath.Join(dir, currentName) }

// WALFile returns the path of the write-ahead log.
func WALFile(dir string) string { return filepath.Join(dir, walName) }

// ReadCurrent returns the active generation. It returns (0, os.ErrNotExist)
// when CURRENT does not yet exist (a fresh, never-checkpointed database).
func ReadCurrent(dir string) (uint64, error) {
	b, err := os.ReadFile(CurrentFile(dir))
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	gen, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("layout: corrupt CURRENT %q: %w", s, err)
	}
	return gen, nil
}

// WriteCurrent atomically points CURRENT at gen (temp file + rename + dir
// fsync). Because rename is atomic, CURRENT is always fully the old or the new
// generation, never torn.
func WriteCurrent(dir string, gen uint64) error {
	tmp := filepath.Join(dir, currentTmp)
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("layout: write CURRENT tmp: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", gen); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, CurrentFile(dir)); err != nil {
		return fmt.Errorf("layout: rename CURRENT: %w", err)
	}
	return FsyncDir(dir)
}

// ListGenerations returns all data-file generations present, ascending.
func ListGenerations(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var gens []uint64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, dataPrefix) || !strings.HasSuffix(name, dataSuffix) {
			continue
		}
		mid := name[len(dataPrefix) : len(name)-len(dataSuffix)]
		if gen, err := strconv.ParseUint(mid, 10, 64); err == nil {
			gens = append(gens, gen)
		}
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i] < gens[j] })
	return gens, nil
}

// FsyncDir flushes a directory entry so a rename/create survives a crash. It is
// a no-op on Windows, where directories cannot be opened as files (there the
// rename itself is durable via the filesystem).
func FsyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("layout: open dir for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("layout: fsync dir: %w", err)
	}
	return nil
}
