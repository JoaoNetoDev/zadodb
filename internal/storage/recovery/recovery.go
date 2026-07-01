// Package recovery reconstructs a consistent starting state at boot. It never
// mutates the active data generation in place (which could tear its meta page
// under a crash); instead it maps the active generation read-only and rebuilds
// the in-memory overlay of not-yet-checkpointed writes by replaying the WAL.
//
// It handles the two post-crash states the checkpoint protocol can leave:
//
//   - Interrupted before CURRENT switched: an orphan data.<G+1>.tmp and/or a
//     leftover wal.applying.log. The orphan temp is discarded; a present
//     wal.applying.log means the checkpoint is completed here (its records are
//     folded into a fresh generation), so no acknowledged write is ever lost.
//   - Completed: CURRENT already names the new generation; any leftover
//     wal.applying.log replays as a no-op and is removed.
//
// In all cases the worst outcome is the loss of writes that were never fsynced
// (torn WAL tail) — never corruption.
package recovery

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/JoaoNetoDev/zadodb/internal/storage/btree"
	"github.com/JoaoNetoDev/zadodb/internal/storage/checkpoint"
	"github.com/JoaoNetoDev/zadodb/internal/storage/idgen"
	"github.com/JoaoNetoDev/zadodb/internal/storage/layout"
	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

// ReplayedEntry is a WAL record that is durable but not yet folded into the
// active data generation. The engine turns these into its read overlay.
type ReplayedEntry struct {
	TxID  uint64
	Entry wal.WALEntry
}

// Result is everything the engine needs to resume after recovery.
type Result struct {
	Dir            string
	ActiveGen      uint64
	ActiveDataPath string
	LastTxID       uint64          // highest TxID seen; the sequencer resumes past it
	Replayed       []ReplayedEntry // overlay of not-yet-checkpointed writes, in order
	Gen            *idgen.Generator
}

// Recover prepares the database directory and returns the resumed state.
func Recover(dir string) (*Result, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("recovery: mkdir %s: %w", dir, err)
	}
	if err := removeTempFiles(dir); err != nil {
		return nil, err
	}

	curGen, err := layout.ReadCurrent(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := initFresh(dir); err != nil {
			return nil, err
		}
		curGen = 0
	} else if err != nil {
		return nil, err
	}

	// Discard orphan generations newer than CURRENT (interrupted checkpoint
	// that never switched).
	if err := removeGenerationsAbove(dir, curGen); err != nil {
		return nil, err
	}

	// A leftover retired WAL means a checkpoint was interrupted; complete it by
	// folding it into a fresh generation. Idempotent if it was already applied.
	retired := filepath.Join(dir, checkpoint.RetiredWALName)
	if fileExists(retired) {
		newGen := curGen + 1
		if _, err := checkpoint.BuildGeneration(dir, curGen, newGen, retired); err != nil {
			return nil, fmt.Errorf("recovery: complete interrupted checkpoint: %w", err)
		}
		if err := os.Remove(retired); err != nil {
			return nil, err
		}
		curGen = newGen
	}

	activePath := layout.DataFile(dir, curGen)
	gen := idgen.New()

	// Read the active generation's meta and reseed the id generator from the
	// object ids already stored there.
	mgr, err := page.Open(activePath)
	if err != nil {
		return nil, err
	}
	meta, err := mgr.ReadMeta()
	if err != nil {
		mgr.Close()
		return nil, err
	}
	if err := observeTreeIDs(mgr, meta.Root, gen); err != nil {
		mgr.Close()
		return nil, err
	}
	mgr.Close()

	// Replay the live WAL into the overlay. Records at or below the active
	// generation's LastAppliedTxID are already folded in and skipped.
	walPath := layout.WALFile(dir)
	replayed, maxTx, durableOffset, err := replayWAL(walPath, meta.LastAppliedTxID, gen)
	if err != nil {
		return nil, err
	}
	// Truncate the WAL to its last valid record. A crash may have left a torn or
	// partial record at (or before) the tail; without truncation, subsequent
	// appends would sit AFTER that garbage and the next recovery would stop at
	// it, silently dropping everything after. Truncating makes appends resume
	// cleanly right past the durable prefix.
	if err := truncateWAL(walPath, durableOffset); err != nil {
		return nil, err
	}

	lastTx := meta.LastAppliedTxID
	if maxTx > lastTx {
		lastTx = maxTx
	}

	return &Result{
		Dir:            dir,
		ActiveGen:      curGen,
		ActiveDataPath: activePath,
		LastTxID:       lastTx,
		Replayed:       replayed,
		Gen:            gen,
	}, nil
}

// initFresh creates an empty generation 0 and an empty WAL.
func initFresh(dir string) error {
	gen0 := layout.DataFile(dir, 0)
	m, err := page.Create(gen0)
	if err != nil {
		return fmt.Errorf("recovery: init gen0: %w", err)
	}
	if err := m.WriteMeta(page.Meta{Root: page.InvalidPageID, NumPages: m.NumPages()}); err != nil {
		m.Close()
		return err
	}
	if err := m.Sync(); err != nil {
		m.Close()
		return err
	}
	m.Close()
	// Create an empty WAL file.
	w, err := os.OpenFile(layout.WALFile(dir), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	w.Close()
	return layout.WriteCurrent(dir, 0)
}

// replayWAL reads records with TxID > skipUpTo into the overlay, observing ids.
// It returns the replayed entries, the highest TxID seen (including skipped
// ones, so ids/txids are never reused), and the durable offset — the byte
// length of the valid prefix (everything before the first torn/corrupt record).
func replayWAL(path string, skipUpTo uint64, gen *idgen.Generator) ([]ReplayedEntry, uint64, int64, error) {
	r, err := wal.OpenReader(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, 0, nil
	}
	if err != nil {
		return nil, 0, 0, err
	}
	defer r.Close()

	var out []ReplayedEntry
	var maxTx uint64
	for {
		txID, payload, e := r.Next()
		if e == io.EOF || errors.Is(e, wal.ErrCorrupt) {
			break // clean end or torn tail: stop replaying
		}
		if e != nil {
			return nil, 0, 0, e
		}
		if txID > maxTx {
			maxTx = txID
		}
		if txID <= skipUpTo {
			continue
		}
		entry, e := wal.UnmarshalEntry(payload)
		if e != nil {
			return nil, 0, 0, fmt.Errorf("recovery: decode entry tx %d: %w", txID, e)
		}
		// A batch expands into its sub-mutations, each carrying the batch's
		// TxID so the overlay and its later pruning treat them as one unit.
		for _, sub := range entry.Flatten() {
			if sub.Op == wal.OpPut && sub.ObjectID > 0 {
				gen.Observe(sub.Class, sub.ObjectID)
			}
			out = append(out, ReplayedEntry{TxID: txID, Entry: sub})
		}
	}
	return out, maxTx, r.Offset(), nil
}

// truncateWAL trims the WAL file to the given durable length, discarding any
// torn tail so future appends resume cleanly.
func truncateWAL(path string, durableOffset int64) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() == durableOffset {
		return nil // nothing torn
	}
	if err := os.Truncate(path, durableOffset); err != nil {
		return fmt.Errorf("recovery: truncate WAL to %d: %w", durableOffset, err)
	}
	return nil
}

// observeTreeIDs scans the active tree and seeds the generator from stored ids.
func observeTreeIDs(src btree.PageSource, root page.PageID, gen *idgen.Generator) error {
	return btree.Scan(src, root, nil, func(key, _ []byte) bool {
		if class, id, ok := wal.DecodeObjectKey(key); ok {
			gen.Observe(class, id)
		}
		return true
	})
}

func removeTempFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

func removeGenerationsAbove(dir string, keep uint64) error {
	gens, err := layout.ListGenerations(dir)
	if err != nil {
		return err
	}
	for _, g := range gens {
		if g > keep {
			_ = os.Remove(layout.DataFile(dir, g))
		}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
