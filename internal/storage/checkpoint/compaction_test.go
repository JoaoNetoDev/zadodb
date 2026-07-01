package checkpoint

import (
	"fmt"
	"os"
	"testing"

	"github.com/JoaoNetoDev/zadodb/internal/storage/layout"
)

// fileSize returns the size of the active data generation file.
func activeDataSize(t *testing.T, dir string) int64 {
	t.Helper()
	gen, err := layout.ReadCurrent(dir)
	if err != nil {
		t.Fatalf("ReadCurrent: %v", err)
	}
	fi, err := os.Stat(layout.DataFile(dir, gen))
	if err != nil {
		t.Fatalf("stat data file: %v", err)
	}
	return fi.Size()
}

// TestCheckpointCompactsAndStaysBounded proves the fix: folding many writes
// produces a data file proportional to the live data, and repeated checkpoints
// do NOT grow it without bound (the old incremental-COW checkpoint exploded).
func TestCheckpointCompactsAndStaysBounded(t *testing.T) {
	dir, seq, mapped := newDB(t)

	// Insert a lot of data, then checkpoint.
	const n = 20000
	for i := 0; i < n; i++ {
		put(t, seq, "Item", int64(i+1), fmt.Sprintf("value-%d-padding-padding-padding", i))
	}
	if _, _, err := Run(dir, seq, mapped); err != nil {
		t.Fatalf("checkpoint 1: %v", err)
	}
	size1 := activeDataSize(t, dir)

	// A compact file should be within a small multiple of the raw data. Each
	// value is ~40 bytes + key/overhead; n*~80 bytes ~= 1.6MB. Allow generous
	// headroom but far below the COW-amplified sizes (which were 10-70x).
	rawApprox := int64(n * 90)
	if size1 > rawApprox*3 {
		t.Fatalf("gen size %d is not compact (raw approx %d)", size1, rawApprox)
	}
	t.Logf("after %d inserts: data file = %d bytes (~%.1fx raw)", n, size1, float64(size1)/float64(rawApprox))

	// Re-checkpoint several times with NO new writes: size must stay ~stable,
	// not grow each round (the old checkpoint copied + re-applied, inflating).
	for round := 0; round < 3; round++ {
		if _, _, err := Run(dir, seq, mapped); err != nil {
			t.Fatalf("checkpoint round %d: %v", round, err)
		}
	}
	size2 := activeDataSize(t, dir)
	if size2 > size1+4096*4 { // allow a few pages of slack
		t.Fatalf("repeated checkpoints grew the file: %d -> %d", size1, size2)
	}
	t.Logf("after 3 empty re-checkpoints: %d bytes (was %d) — stable", size2, size1)

	// Data is intact through all the compaction.
	for _, id := range []int64{1, n / 2, n} {
		got, found := mustGet(t, mapped, "Item", id)
		want := fmt.Sprintf("value-%d-padding-padding-padding", id-1)
		if !found || got != want {
			t.Fatalf("Item/%d = (%q,%v), want %q", id, got, found, want)
		}
	}
}

// TestCheckpointCompactsDeletes confirms deleted keys leave no residue after a
// compacting checkpoint (dead space is reclaimed, not carried forward).
func TestCheckpointCompactsDeletes(t *testing.T) {
	dir, seq, mapped := newDB(t)
	const n = 5000
	for i := 0; i < n; i++ {
		put(t, seq, "Item", int64(i+1), fmt.Sprintf("v%d", i))
	}
	if _, _, err := Run(dir, seq, mapped); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	full := activeDataSize(t, dir)

	// Delete almost everything, then checkpoint again.
	for i := 0; i < n-10; i++ {
		del(t, seq, "Item", int64(i+1))
	}
	if _, _, err := Run(dir, seq, mapped); err != nil {
		t.Fatalf("checkpoint 2: %v", err)
	}
	compacted := activeDataSize(t, dir)

	if compacted >= full {
		t.Fatalf("after deleting most rows, file did not shrink: %d -> %d", full, compacted)
	}
	t.Logf("delete compaction: %d -> %d bytes", full, compacted)

	// The 10 survivors remain.
	for id := int64(n - 9); id <= n; id++ {
		if _, found := mustGet(t, mapped, "Item", id); !found {
			t.Fatalf("survivor Item/%d missing", id)
		}
	}
	// A deleted one is gone.
	if _, found := mustGet(t, mapped, "Item", 1); found {
		t.Fatalf("deleted Item/1 still present")
	}
}
