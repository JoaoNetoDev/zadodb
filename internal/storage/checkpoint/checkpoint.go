// Package checkpoint folds accumulated WAL records into a fresh data-file
// generation and publishes it atomically.
//
// The sequence is crash-safe at every step (see recovery for the mirror image):
//
//  1. Rotate the WAL: cut it at a clean record boundary into wal.applying.log;
//     new writes go to a fresh wal.log.
//  2. Build data.<G+1>.zdb.tmp from the current generation + the retired WAL
//     (copy-on-write inserts/deletes), stamping LastAppliedTxID into its meta.
//  3. fsync and rename the temp to its final generation name (a brand-new file
//     name, so even on Windows nothing mapped is replaced).
//  4. WriteCurrent(G+1): the atomic point at which the new generation becomes
//     the truth.
//  5. Swap the read snapshot, then delete the now-redundant wal.applying.log.
//
// A crash before step 4 leaves CURRENT at G with an orphan G+1 (discarded at
// recovery). A crash after step 4 leaves a complete G+1 whose replay of the
// leftover wal.applying.log is a no-op (all its TxIDs are already applied).
package checkpoint

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/JoaoNetoDev/zadodb/internal/storage/btree"
	"github.com/JoaoNetoDev/zadodb/internal/storage/layout"
	"github.com/JoaoNetoDev/zadodb/internal/storage/mvcc"
	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

// Run performs one checkpoint and returns the new generation number and the
// highest TxID folded into it (so the engine can prune its overlay). seq is the
// live sequencer (whose WAL is rotated); mapped is the read snapshot to swap.
func Run(dir string, seq *wal.Sequencer, mapped *mvcc.MappedFile) (newGen, foldedTxID uint64, err error) {
	curGen, err := layout.ReadCurrent(dir)
	if err != nil {
		return 0, 0, fmt.Errorf("checkpoint: read CURRENT: %w", err)
	}
	newGen = curGen + 1
	retired := filepath.Join(dir, RetiredWALName)

	// 1. Cut the WAL. Everything to fold in is now in retired.
	if err := seq.Rotate(retired); err != nil {
		return 0, 0, fmt.Errorf("checkpoint: rotate wal: %w", err)
	}

	// 2-4. Build + publish + switch CURRENT to the new generation.
	foldedTxID, err = BuildGeneration(dir, curGen, newGen, retired)
	if err != nil {
		return 0, 0, err
	}

	// 5. Point readers at the new generation, then drop the retired WAL.
	if err := mapped.SwapTo(layout.DataFile(dir, newGen)); err != nil {
		return 0, 0, err
	}
	_ = os.Remove(retired)

	cleanupOldGenerations(dir, newGen)
	return newGen, foldedTxID, nil
}

// RetiredWALName is the fixed name of the WAL segment being folded by a
// checkpoint. Its presence at boot signals an interrupted checkpoint that
// recovery must complete.
const RetiredWALName = "wal.applying.log"

// BuildGeneration constructs data.<newGen> from data.<baseGen> plus the records
// in retiredWAL, then atomically publishes it and points CURRENT at it. It
// returns the highest TxID folded in. It does not touch the live sequencer or
// read snapshot, so recovery can reuse it to finish an interrupted checkpoint.
//
// The new generation is built by COMPACTION: the base tree is streamed in key
// order and merged with the WAL deltas into a fresh, bulk-loaded B+Tree. This
// keeps the file proportional to the live data — unlike incremental COW
// application, which amplifies size without bound.
func BuildGeneration(dir string, baseGen, newGen uint64, retiredWAL string) (uint64, error) {
	base := layout.DataFile(dir, baseGen)
	newTmp := layout.DataFile(dir, newGen) + ".tmp"

	// Read the base generation's root and applied position (read-only).
	baseMgr, err := page.Open(base)
	if err != nil {
		return 0, err
	}
	baseMeta, err := baseMgr.ReadMeta()
	if err != nil {
		baseMgr.Close()
		return 0, err
	}

	// Collect the net WAL deltas beyond what the base already contains.
	deltas, sortedKeys, lastApplied, err := parseWALDeltas(retiredWAL, baseMeta.LastAppliedTxID)
	if err != nil {
		baseMgr.Close()
		return 0, err
	}

	// Build the compacted generation.
	if err := os.Remove(newTmp); err != nil && !os.IsNotExist(err) {
		baseMgr.Close()
		return 0, err
	}
	mgr, err := page.Create(newTmp)
	if err != nil {
		baseMgr.Close()
		return 0, err
	}
	root, err := mergeBuild(mgr, baseMgr, baseMeta.Root, deltas, sortedKeys)
	baseMgr.Close()
	if err != nil {
		mgr.Close()
		return 0, err
	}

	if err := mgr.WriteMeta(page.Meta{Root: root, LastAppliedTxID: lastApplied, NumPages: mgr.NumPages()}); err != nil {
		mgr.Close()
		return 0, err
	}
	if err := mgr.Sync(); err != nil {
		mgr.Close()
		return 0, err
	}
	mgr.Close()

	finalData := layout.DataFile(dir, newGen)
	if err := os.Rename(newTmp, finalData); err != nil {
		return 0, fmt.Errorf("checkpoint: publish generation: %w", err)
	}
	if err := layout.FsyncDir(dir); err != nil {
		return 0, err
	}
	// The atomic point at which the new generation becomes the truth.
	if err := layout.WriteCurrent(dir, newGen); err != nil {
		return 0, err
	}
	return lastApplied, nil
}

// delta is the net effect of the WAL on a single key.
type delta struct {
	data    []byte
	deleted bool
}

// parseWALDeltas reads the retired WAL and returns the net per-key deltas beyond
// baseApplied, their keys sorted ascending, and the highest TxID seen (so ids
// and txids are never reused even for records already folded into the base).
func parseWALDeltas(retired string, baseApplied uint64) (map[string]delta, [][]byte, uint64, error) {
	deltas := make(map[string]delta)
	lastApplied := baseApplied

	r, err := wal.OpenReader(retired)
	if err != nil {
		return nil, nil, 0, err
	}
	defer r.Close()
	for {
		txID, payload, e := r.Next()
		if e == io.EOF || e == wal.ErrCorrupt {
			break // clean or torn tail: end of the durable prefix
		}
		if e != nil {
			return nil, nil, 0, e
		}
		if txID > lastApplied {
			lastApplied = txID
		}
		if txID <= baseApplied {
			continue // already folded into the base
		}
		entry, e := wal.UnmarshalEntry(payload)
		if e != nil {
			return nil, nil, 0, fmt.Errorf("checkpoint: decode entry tx %d: %w", txID, e)
		}
		// Flatten batches; later records overwrite earlier ones for a key.
		for _, sub := range entry.Flatten() {
			switch sub.Op {
			case wal.OpDelete, wal.OpDropClass, wal.OpDropRel:
				deltas[string(sub.Key())] = delta{deleted: true}
			default:
				deltas[string(sub.Key())] = delta{data: sub.Data}
			}
		}
	}

	sorted := make([][]byte, 0, len(deltas))
	for k := range deltas {
		sorted = append(sorted, []byte(k))
	}
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i], sorted[j]) < 0 })
	return deltas, sorted, lastApplied, nil
}

// mergeBuild streams the base tree in key order, merges the sorted WAL deltas on
// top (deltas win; deletes drop keys), and bulk-loads the result into a fresh
// compact B+Tree, returning its root. Memory is bounded to the deltas plus the
// builder's per-level index — the base tree is streamed, never materialized.
func mergeBuild(store btree.PageStore, base btree.PageSource, baseRoot page.PageID, deltas map[string]delta, sortedKeys [][]byte) (page.PageID, error) {
	b := btree.NewBuilder(store)
	di := 0

	flushDeltasBefore := func(key []byte) error {
		for di < len(sortedKeys) && bytes.Compare(sortedKeys[di], key) < 0 {
			dk := sortedKeys[di]
			d := deltas[string(dk)]
			di++
			if d.deleted {
				continue
			}
			if err := b.Add(dk, d.data); err != nil {
				return err
			}
		}
		return nil
	}

	var scanErr error
	err := btree.Scan(base, baseRoot, nil, func(key, val []byte) bool {
		if scanErr = flushDeltasBefore(key); scanErr != nil {
			return false
		}
		if di < len(sortedKeys) && bytes.Equal(sortedKeys[di], key) {
			d := deltas[string(sortedKeys[di])]
			di++
			if !d.deleted {
				if scanErr = b.Add(key, d.data); scanErr != nil {
					return false
				}
			}
			return true // delta overrides (or deletes) the base value
		}
		if scanErr = b.Add(key, val); scanErr != nil {
			return false
		}
		return true
	})
	if err != nil {
		return 0, err
	}
	if scanErr != nil {
		return 0, scanErr
	}
	// Remaining deltas are keys greater than everything in the base.
	for di < len(sortedKeys) {
		dk := sortedKeys[di]
		d := deltas[string(dk)]
		di++
		if d.deleted {
			continue
		}
		if err := b.Add(dk, d.data); err != nil {
			return 0, err
		}
	}
	return b.Finish()
}

// cleanupOldGenerations removes data files older than keep. Failures (e.g. a
// still-mapped file on Windows) are ignored and retried on a later pass.
func cleanupOldGenerations(dir string, keep uint64) {
	gens, err := layout.ListGenerations(dir)
	if err != nil {
		return
	}
	for _, g := range gens {
		if g < keep {
			_ = os.Remove(layout.DataFile(dir, g))
		}
	}
}
