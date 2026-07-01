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
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/JoaoNetoDev/zadodb/internal/storage/apply"
	"github.com/JoaoNetoDev/zadodb/internal/storage/btree"
	"github.com/JoaoNetoDev/zadodb/internal/storage/layout"
	"github.com/JoaoNetoDev/zadodb/internal/storage/mvcc"
	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

// Run performs one checkpoint and returns the new generation number. seq is the
// live sequencer (whose WAL is rotated); mapped is the read snapshot to swap.
func Run(dir string, seq *wal.Sequencer, mapped *mvcc.MappedFile) (uint64, error) {
	curGen, err := layout.ReadCurrent(dir)
	if err != nil {
		return 0, fmt.Errorf("checkpoint: read CURRENT: %w", err)
	}
	newGen := curGen + 1
	retired := filepath.Join(dir, "wal.applying.log")

	// 1. Cut the WAL. Everything to fold in is now in retired.
	if err := seq.Rotate(retired); err != nil {
		return 0, fmt.Errorf("checkpoint: rotate wal: %w", err)
	}

	// 2. Build the new generation from the base file + the retired WAL.
	base := layout.DataFile(dir, curGen)
	newTmp := layout.DataFile(dir, newGen) + ".tmp"
	if err := copyFile(base, newTmp); err != nil {
		return 0, fmt.Errorf("checkpoint: copy base: %w", err)
	}
	lastApplied, root, npages, err := foldWAL(newTmp, retired)
	if err != nil {
		return 0, err
	}

	// 3. Persist meta, fsync, and publish under the final generation name.
	mgr, err := page.Open(newTmp)
	if err != nil {
		return 0, err
	}
	if err := mgr.WriteMeta(page.Meta{Root: root, LastAppliedTxID: lastApplied, NumPages: npages}); err != nil {
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

	// 4. Atomically switch to the new generation.
	if err := layout.WriteCurrent(dir, newGen); err != nil {
		return 0, err
	}

	// 5. Point readers at the new generation, then drop the retired WAL.
	if err := mapped.SwapTo(finalData); err != nil {
		return 0, err
	}
	_ = os.Remove(retired)

	cleanupOldGenerations(dir, newGen)
	return newGen, nil
}

// foldWAL opens the temp data file, applies every retired WAL record with a
// TxID beyond what the base already contains, and returns the resulting state.
func foldWAL(dataTmp, retired string) (lastApplied uint64, root page.PageID, npages uint64, err error) {
	mgr, err := page.Open(dataTmp)
	if err != nil {
		return 0, 0, 0, err
	}
	defer mgr.Close()
	meta, err := mgr.ReadMeta()
	if err != nil {
		return 0, 0, 0, err
	}
	tree := btree.Load(mgr, meta.Root)
	lastApplied = meta.LastAppliedTxID

	r, err := wal.OpenReader(retired)
	if err != nil {
		return 0, 0, 0, err
	}
	defer r.Close()
	for {
		txID, payload, e := r.Next()
		if e == io.EOF || e == wal.ErrCorrupt {
			break // clean or torn tail: end of the durable prefix
		}
		if e != nil {
			return 0, 0, 0, e
		}
		if txID <= lastApplied {
			continue // idempotent: already folded in
		}
		entry, e := wal.UnmarshalEntry(payload)
		if e != nil {
			return 0, 0, 0, fmt.Errorf("checkpoint: decode entry tx %d: %w", txID, e)
		}
		if e := apply.Entry(tree, entry); e != nil {
			return 0, 0, 0, e
		}
		lastApplied = txID
	}
	return lastApplied, tree.Root(), mgr.NumPages(), nil
}

// copyFile copies src to dst (truncating dst) and fsyncs dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
