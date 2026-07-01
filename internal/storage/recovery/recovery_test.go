package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JoaoNetoDev/zadodb/internal/storage/layout"
	"github.com/JoaoNetoDev/zadodb/internal/storage/mvcc"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

func writeWAL(t *testing.T, path string, entries []wal.WALEntry, startTx uint64) int64 {
	t.Helper()
	w, err := wal.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	for i, e := range entries {
		p, err := e.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if err := w.Append(startTx+uint64(i), p); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	w.Sync()
	off := w.Offset()
	w.Close()
	return off
}

func TestRecoverFreshInit(t *testing.T) {
	dir := t.TempDir()
	res, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.ActiveGen != 0 || res.LastTxID != 0 || len(res.Replayed) != 0 {
		t.Fatalf("fresh recover = %+v, want gen0/tx0/no replay", res)
	}
	if cur, _ := layout.ReadCurrent(dir); cur != 0 {
		t.Fatalf("CURRENT = %d, want 0", cur)
	}
	if _, err := os.Stat(layout.DataFile(dir, 0)); err != nil {
		t.Fatalf("gen0 data file missing: %v", err)
	}
}

func TestRecoverReplaysWAL(t *testing.T) {
	dir := t.TempDir()
	if _, err := Recover(dir); err != nil { // init gen0 + empty wal
		t.Fatalf("init Recover: %v", err)
	}
	writeWAL(t, layout.WALFile(dir), []wal.WALEntry{
		{Op: wal.OpPut, Class: "Pessoa", ObjectID: 1, Data: []byte("alice")},
		{Op: wal.OpPut, Class: "Pessoa", ObjectID: 2, Data: []byte("bob")},
		{Op: wal.OpPut, Class: "Filial", ObjectID: 1, Data: []byte("matriz")},
	}, 1)

	res, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Replayed) != 3 {
		t.Fatalf("replayed %d, want 3", len(res.Replayed))
	}
	if res.LastTxID != 3 {
		t.Fatalf("LastTxID = %d, want 3", res.LastTxID)
	}
	// The id generator resumed past the highest observed id per class.
	if got := res.Gen.Next(wal.ScopeKey("", "Pessoa")); got != 3 {
		t.Fatalf("next Pessoa id = %d, want 3", got)
	}
	if got := res.Gen.Next(wal.ScopeKey("", "Filial")); got != 2 {
		t.Fatalf("next Filial id = %d, want 2", got)
	}
}

func TestRecoverStopsAtTornTail(t *testing.T) {
	dir := t.TempDir()
	Recover(dir)
	goodOff := writeWAL(t, layout.WALFile(dir), []wal.WALEntry{
		{Op: wal.OpPut, Class: "A", ObjectID: 1, Data: []byte("keep")},
	}, 1)
	// Append a second record then chop it mid-way (simulated crash).
	off2 := writeWAL(t, layout.WALFile(dir), []wal.WALEntry{
		{Op: wal.OpPut, Class: "A", ObjectID: 2, Data: []byte("this will be truncated")},
	}, 2)
	_ = off2
	if err := os.Truncate(layout.WALFile(dir), goodOff+8); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	res, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Replayed) != 1 || string(res.Replayed[0].Entry.Data) != "keep" {
		t.Fatalf("replayed = %+v, want single 'keep' record", res.Replayed)
	}
}

// TestRecoverInterruptedCheckpointBeforeSwitch models the crash state where a
// checkpoint rotated the WAL (leaving wal.applying.log) but crashed before
// switching CURRENT. Recovery must complete the checkpoint so no acknowledged
// write is lost.
func TestRecoverInterruptedCheckpointBeforeSwitch(t *testing.T) {
	dir := t.TempDir()
	Recover(dir) // gen0 empty, CURRENT=0

	// Simulate the rotated segment: records live only in wal.applying.log.
	retired := filepath.Join(dir, "wal.applying.log")
	writeWAL(t, retired, []wal.WALEntry{
		{Op: wal.OpPut, Class: "Pessoa", ObjectID: 1, Data: []byte("alice")},
		{Op: wal.OpPut, Class: "Pessoa", ObjectID: 2, Data: []byte("bob")},
	}, 1)

	res, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// Checkpoint completed: CURRENT advanced, applying.log gone.
	if res.ActiveGen != 1 {
		t.Fatalf("ActiveGen = %d, want 1", res.ActiveGen)
	}
	if _, err := os.Stat(retired); !os.IsNotExist(err) {
		t.Fatalf("wal.applying.log should be gone")
	}
	// The formerly-unapplied records are now durably in the data file.
	assertGet(t, res.ActiveDataPath, "Pessoa", 1, "alice")
	assertGet(t, res.ActiveDataPath, "Pessoa", 2, "bob")
	// Nothing left in the overlay (all folded in).
	if len(res.Replayed) != 0 {
		t.Fatalf("replayed = %d, want 0 (all folded)", len(res.Replayed))
	}
	// Generator reseeded from the data on disk.
	if got := res.Gen.Next(wal.ScopeKey("", "Pessoa")); got != 3 {
		t.Fatalf("next Pessoa id = %d, want 3", got)
	}
}

// TestRecoverInterruptedCheckpointAfterSwitch models the crash state where the
// checkpoint had already switched CURRENT but crashed before deleting
// wal.applying.log. Replaying it must be a harmless no-op.
func TestRecoverInterruptedCheckpointAfterSwitch(t *testing.T) {
	dir := t.TempDir()
	Recover(dir)

	// First, do a real fold to produce gen1 with the records applied.
	retired := filepath.Join(dir, "wal.applying.log")
	writeWAL(t, retired, []wal.WALEntry{
		{Op: wal.OpPut, Class: "Pessoa", ObjectID: 1, Data: []byte("alice")},
	}, 1)
	res, err := Recover(dir) // completes -> gen1, applying.log removed
	if err != nil {
		t.Fatalf("first Recover: %v", err)
	}
	if res.ActiveGen != 1 {
		t.Fatalf("ActiveGen = %d, want 1", res.ActiveGen)
	}

	// Now re-plant a stale applying.log with the same (already-applied) record
	// and recover again: it should fold to gen2 as a no-op and stay consistent.
	writeWAL(t, retired, []wal.WALEntry{
		{Op: wal.OpPut, Class: "Pessoa", ObjectID: 1, Data: []byte("alice")},
	}, 1)
	res2, err := Recover(dir)
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if res2.ActiveGen != 2 {
		t.Fatalf("ActiveGen = %d, want 2", res2.ActiveGen)
	}
	assertGet(t, res2.ActiveDataPath, "Pessoa", 1, "alice")
}

// TestRecoverTruncatesTornTailThenAppendsCleanly reproduces the compound bug
// that data can be lost if a torn tail is not truncated: after a crash leaves a
// partial record, the next session appends AFTER it, and a later recovery would
// stop at the torn record and drop everything after. Recovery must truncate the
// WAL to its durable prefix so appends resume cleanly.
func TestRecoverTruncatesTornTailThenAppendsCleanly(t *testing.T) {
	dir := t.TempDir()
	Recover(dir) // init gen0 + empty wal

	walPath := layout.WALFile(dir)
	// One good record, then a partial (torn) tail.
	goodOff := writeWAL(t, walPath, []wal.WALEntry{
		{Op: wal.OpPut, Class: "A", ObjectID: 1, Data: []byte("keep")},
	}, 1)
	writeWAL(t, walPath, []wal.WALEntry{
		{Op: wal.OpPut, Class: "A", ObjectID: 2, Data: []byte("partial")},
	}, 2)
	// Chop mid-second-record to simulate a torn write.
	if err := os.Truncate(walPath, goodOff+10); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	// First recovery must truncate the WAL back to the good prefix.
	res, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Replayed) != 1 {
		t.Fatalf("replayed %d, want 1", len(res.Replayed))
	}
	if info, _ := os.Stat(walPath); info.Size() != goodOff {
		t.Fatalf("WAL not truncated: size %d, want %d", info.Size(), goodOff)
	}

	// Append a new record AFTER truncation (as a fresh session would).
	writeWAL(t, walPath, []wal.WALEntry{
		{Op: wal.OpPut, Class: "A", ObjectID: 2, Data: []byte("clean")},
	}, res.LastTxID+1)

	// Second recovery must see BOTH records — proving the append landed on a
	// clean boundary, not after garbage.
	res2, err := Recover(dir)
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if len(res2.Replayed) != 2 {
		t.Fatalf("after clean append, replayed %d, want 2", len(res2.Replayed))
	}
}

// TestRecoverBatchIsAllOrNothing proves batch atomicity: a torn batch record is
// dropped entirely on recovery, never applied partially.
func TestRecoverBatchIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	Recover(dir)
	walPath := layout.WALFile(dir)

	// A committed single put, then a batch of three.
	goodOff := writeWAL(t, walPath, []wal.WALEntry{
		{Op: wal.OpPut, Class: "A", ObjectID: 1, Data: []byte("solo")},
	}, 1)
	batch := wal.WALEntry{Op: wal.OpBatch, Sub: []wal.WALEntry{
		{Op: wal.OpPut, Class: "A", ObjectID: 2, Data: []byte("b2")},
		{Op: wal.OpPut, Class: "A", ObjectID: 3, Data: []byte("b3")},
		{Op: wal.OpPut, Class: "A", ObjectID: 4, Data: []byte("b4")},
	}}
	writeWAL(t, walPath, []wal.WALEntry{batch}, 2)
	// Tear the batch record mid-way (simulate a crash during its write).
	if err := os.Truncate(walPath, goodOff+12); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	res, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// Only the solo put survives; NONE of the batch's three entries applied.
	if len(res.Replayed) != 1 {
		t.Fatalf("replayed %d entries, want 1 (torn batch must be dropped whole)", len(res.Replayed))
	}
	if res.Replayed[0].Entry.ObjectID != 1 {
		t.Fatalf("surviving entry id = %d, want 1", res.Replayed[0].Entry.ObjectID)
	}
}

// TestRecoverCompleteBatchAppliesAll confirms a fully-written batch record
// replays all its sub-entries.
func TestRecoverCompleteBatchAppliesAll(t *testing.T) {
	dir := t.TempDir()
	Recover(dir)
	walPath := layout.WALFile(dir)
	batch := wal.WALEntry{Op: wal.OpBatch, Sub: []wal.WALEntry{
		{Op: wal.OpPut, Class: "A", ObjectID: 1, Data: []byte("b1")},
		{Op: wal.OpPut, Class: "A", ObjectID: 2, Data: []byte("b2")},
	}}
	writeWAL(t, walPath, []wal.WALEntry{batch}, 1)

	res, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(res.Replayed) != 2 {
		t.Fatalf("replayed %d, want 2 (whole batch)", len(res.Replayed))
	}
	// Generator reseeded from both ids.
	if got := res.Gen.Next(wal.ScopeKey("", "A")); got != 3 {
		t.Fatalf("next id = %d, want 3", got)
	}
}

func assertGet(t *testing.T, dataPath, class string, id int64, want string) {
	t.Helper()
	mf, err := mvcc.Open(dataPath)
	if err != nil {
		t.Fatalf("mvcc.Open: %v", err)
	}
	defer mf.Close()
	snap := mf.Acquire()
	defer snap.Release()
	got, found, err := snap.Get(wal.ObjectKey("", class, id))
	if err != nil || !found {
		t.Fatalf("Get %s/%d: found %v err %v", class, id, found, err)
	}
	if string(got) != want {
		t.Fatalf("Get %s/%d = %q, want %q", class, id, got, want)
	}
}
