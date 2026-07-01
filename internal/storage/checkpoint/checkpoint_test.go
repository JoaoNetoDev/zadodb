package checkpoint

import (
	"fmt"
	"os"
	"testing"

	"github.com/JoaoNetoDev/zadodb/internal/storage/layout"
	"github.com/JoaoNetoDev/zadodb/internal/storage/mvcc"
	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

// newDB bootstraps a fresh, empty generation-0 database in a temp dir and
// returns (dir, sequencer, mappedFile).
func newDB(t *testing.T) (string, *wal.Sequencer, *mvcc.MappedFile) {
	t.Helper()
	dir := t.TempDir()

	// Empty gen 0.
	gen0 := layout.DataFile(dir, 0)
	m, err := page.Create(gen0)
	if err != nil {
		t.Fatalf("Create gen0: %v", err)
	}
	if err := m.WriteMeta(page.Meta{Root: page.InvalidPageID, NumPages: m.NumPages()}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	m.Sync()
	m.Close()
	if err := layout.WriteCurrent(dir, 0); err != nil {
		t.Fatalf("WriteCurrent: %v", err)
	}

	w, err := wal.OpenWriter(layout.WALFile(dir))
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	seq := wal.NewSequencer(w, wal.DefaultFsyncMode(), 0, 1024)

	mapped, err := mvcc.Open(gen0)
	if err != nil {
		t.Fatalf("mvcc.Open: %v", err)
	}
	t.Cleanup(func() {
		seq.Close()
		mapped.Close()
	})
	return dir, seq, mapped
}

func put(t *testing.T, seq *wal.Sequencer, class string, id int64, data string) {
	t.Helper()
	e := wal.WALEntry{Op: wal.OpPut, Class: class, ObjectID: id, Data: []byte(data)}
	payload, err := e.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := seq.Submit(payload); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

func del(t *testing.T, seq *wal.Sequencer, class string, id int64) {
	t.Helper()
	e := wal.WALEntry{Op: wal.OpDelete, Class: class, ObjectID: id}
	payload, _ := e.Marshal()
	if _, err := seq.Submit(payload); err != nil {
		t.Fatalf("Submit delete: %v", err)
	}
}

func mustGet(t *testing.T, mapped *mvcc.MappedFile, class string, id int64) (string, bool) {
	t.Helper()
	snap := mapped.Acquire()
	defer snap.Release()
	v, found, err := snap.Get(wal.ObjectKey("", class, id))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return string(v), found
}

func TestCheckpointFoldsWAL(t *testing.T) {
	dir, seq, mapped := newDB(t)

	put(t, seq, "Pessoa", 1, "alice")
	put(t, seq, "Pessoa", 2, "bob")
	put(t, seq, "Filial", 1, "matriz")

	gen, _, err := Run(dir, seq, mapped)
	if err != nil {
		t.Fatalf("checkpoint Run: %v", err)
	}
	if gen != 1 {
		t.Fatalf("newGen = %d, want 1", gen)
	}

	// CURRENT advanced.
	if cur, _ := layout.ReadCurrent(dir); cur != 1 {
		t.Fatalf("CURRENT = %d, want 1", cur)
	}
	// The retired WAL was cleaned up.
	if _, err := os.Stat(dir + "/wal.applying.log"); !os.IsNotExist(err) {
		t.Fatalf("wal.applying.log should be gone")
	}
	// Data is visible through the swapped snapshot.
	for _, tc := range []struct {
		class string
		id    int64
		want  string
	}{{"Pessoa", 1, "alice"}, {"Pessoa", 2, "bob"}, {"Filial", 1, "matriz"}} {
		got, found := mustGet(t, mapped, tc.class, tc.id)
		if !found || got != tc.want {
			t.Fatalf("%s/%d = (%q,%v), want %q", tc.class, tc.id, got, found, tc.want)
		}
	}
	// Meta LastAppliedTxID reflects the last folded record.
	m, _ := page.Open(layout.DataFile(dir, 1))
	meta, _ := m.ReadMeta()
	m.Close()
	if meta.LastAppliedTxID != 3 {
		t.Fatalf("LastAppliedTxID = %d, want 3", meta.LastAppliedTxID)
	}
}

func TestCheckpointAccumulatesAndDeletes(t *testing.T) {
	dir, seq, mapped := newDB(t)

	put(t, seq, "Pessoa", 1, "alice")
	if _, _, err := Run(dir, seq, mapped); err != nil {
		t.Fatalf("checkpoint 1: %v", err)
	}

	// A second batch: add, update, delete across a checkpoint.
	put(t, seq, "Pessoa", 2, "bob")
	put(t, seq, "Pessoa", 1, "alice-v2") // update
	del(t, seq, "Pessoa", 2)             // delete what we just added

	gen, _, err := Run(dir, seq, mapped)
	if err != nil {
		t.Fatalf("checkpoint 2: %v", err)
	}
	if gen != 2 {
		t.Fatalf("gen = %d, want 2", gen)
	}

	if got, found := mustGet(t, mapped, "Pessoa", 1); !found || got != "alice-v2" {
		t.Fatalf("Pessoa/1 = (%q,%v), want alice-v2", got, found)
	}
	if _, found := mustGet(t, mapped, "Pessoa", 2); found {
		t.Fatalf("Pessoa/2 should have been deleted")
	}
}

func TestCheckpointEmptyWAL(t *testing.T) {
	// Checkpointing with no pending writes must still succeed and advance.
	dir, seq, mapped := newDB(t)
	gen, _, err := Run(dir, seq, mapped)
	if err != nil {
		t.Fatalf("checkpoint empty: %v", err)
	}
	if gen != 1 {
		t.Fatalf("gen = %d, want 1", gen)
	}
	_ = fmt.Sprint(gen)
}
