// Package apply folds a WAL entry into a B+Tree. It is shared by the checkpoint
// path (building a new data generation) and the recovery path (replaying the
// WAL into the active generation), so both interpret entries identically.
package apply

import (
	"fmt"

	"github.com/JoaoNetoDev/zadodb/internal/storage/btree"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

// Entry applies one WAL entry to the tree. An OpBatch is applied as its
// constituent sub-mutations (all folded into the same tree state).
//
// Class drops are only issued for empty classes (enforced at the engine layer),
// so OpDropClass simply removes the class-definition key.
func Entry(tree *btree.Tree, e wal.WALEntry) error {
	for _, sub := range e.Flatten() {
		if err := one(tree, sub); err != nil {
			return err
		}
	}
	return nil
}

func one(tree *btree.Tree, e wal.WALEntry) error {
	switch e.Op {
	case wal.OpPut, wal.OpCreateClass:
		return tree.Insert(e.Key(), e.Data)
	case wal.OpDelete, wal.OpDropClass:
		return tree.Delete(e.Key())
	default:
		return fmt.Errorf("apply: unknown op %d", e.Op)
	}
}
