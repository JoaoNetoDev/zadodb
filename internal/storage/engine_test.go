package storage

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

func openEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	e, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestEngineCRUDLifecycle(t *testing.T) {
	e := openEngine(t, t.TempDir())

	if err := e.CreateClass("Pessoa"); err != nil {
		t.Fatalf("CreateClass: %v", err)
	}
	if err := e.CreateClass("Pessoa"); !errors.Is(err, ErrClassExists) {
		t.Fatalf("duplicate CreateClass = %v, want ErrClassExists", err)
	}

	id, err := e.CreateObject("Pessoa", []byte("alice"))
	if err != nil {
		t.Fatalf("CreateObject: %v", err)
	}
	if id != 1 {
		t.Fatalf("first id = %d, want 1", id)
	}

	// Read-after-write from the overlay.
	got, found, err := e.GetObject("Pessoa", 1)
	if err != nil || !found || string(got) != "alice" {
		t.Fatalf("GetObject = (%q,%v,%v), want alice", got, found, err)
	}

	// Update, then re-read.
	if err := e.PutObject("Pessoa", 1, []byte("alice-v2")); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got, _, _ = e.GetObject("Pessoa", 1)
	if string(got) != "alice-v2" {
		t.Fatalf("after update = %q, want alice-v2", got)
	}

	// Delete.
	if err := e.DeleteObject("Pessoa", 1); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, found, _ := e.GetObject("Pessoa", 1); found {
		t.Fatalf("object should be gone")
	}
	// Deleting again is a 404.
	if err := e.DeleteObject("Pessoa", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v, want ErrNotFound", err)
	}
}

func TestEngineErrorsForMissingClass(t *testing.T) {
	e := openEngine(t, t.TempDir())
	if _, err := e.CreateObject("Ghost", []byte("x")); !errors.Is(err, ErrNoClass) {
		t.Fatalf("CreateObject no class = %v, want ErrNoClass", err)
	}
	if err := e.CreateClass("bad name!"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("invalid class name = %v, want ErrInvalidName", err)
	}
}

func TestEngineReadAfterCheckpoint(t *testing.T) {
	e := openEngine(t, t.TempDir())
	e.CreateClass("Pessoa")
	for i := 0; i < 50; i++ {
		if _, err := e.CreateObject("Pessoa", []byte(fmt.Sprintf("p%d", i))); err != nil {
			t.Fatalf("CreateObject: %v", err)
		}
	}
	// Force a checkpoint: overlay should be pruned, data still readable via mmap.
	if err := e.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if s := e.Stats(); s.OverlaySize != 0 {
		t.Fatalf("overlay size after checkpoint = %d, want 0", s.OverlaySize)
	}
	for i := 0; i < 50; i++ {
		got, found, err := e.GetObject("Pessoa", int64(i+1))
		if err != nil || !found || string(got) != fmt.Sprintf("p%d", i) {
			t.Fatalf("post-checkpoint Get %d = (%q,%v,%v)", i+1, got, found, err)
		}
	}

	// Writes after a checkpoint go to the overlay again and merge on read.
	id, _ := e.CreateObject("Pessoa", []byte("after"))
	got, found, _ := e.GetObject("Pessoa", id)
	if !found || string(got) != "after" {
		t.Fatalf("post-checkpoint write not visible")
	}
}

func TestEngineListPaginationAndMerge(t *testing.T) {
	e := openEngine(t, t.TempDir())
	e.CreateClass("Item")
	for i := 0; i < 10; i++ {
		e.CreateObject("Item", []byte(fmt.Sprintf("v%d", i)))
	}
	e.Checkpoint() // ids 1..10 now in the snapshot
	// Add more (overlay) and delete one from the snapshot.
	e.CreateObject("Item", []byte("v10")) // id 11
	e.DeleteObject("Item", 5)

	all, err := e.ListObjects("Item", 0, 0)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(all) != 10 { // 11 created total, minus the deleted id 5
		t.Fatalf("list len = %d, want 10", len(all))
	}
	// Ascending ids, id 5 absent.
	for i := 1; i < len(all); i++ {
		if all[i-1].ID >= all[i].ID {
			t.Fatalf("not ascending at %d", i)
		}
		if all[i].ID == 5 {
			t.Fatalf("deleted id 5 present")
		}
	}
	// Pagination.
	page, _ := e.ListObjects("Item", 3, 2)
	if len(page) != 3 {
		t.Fatalf("paginated len = %d, want 3", len(page))
	}
}

func TestEngineDropClass(t *testing.T) {
	e := openEngine(t, t.TempDir())
	e.CreateClass("Temp")
	id, _ := e.CreateObject("Temp", []byte("x"))
	if err := e.DropClass("Temp"); !errors.Is(err, ErrClassNotEmpty) {
		t.Fatalf("drop non-empty = %v, want ErrClassNotEmpty", err)
	}
	e.DeleteObject("Temp", id)
	if err := e.DropClass("Temp"); err != nil {
		t.Fatalf("DropClass: %v", err)
	}
	if e.ClassExists("Temp") {
		t.Fatalf("class still exists after drop")
	}
	if err := e.DropClass("Temp"); !errors.Is(err, ErrNoClass) {
		t.Fatalf("drop missing = %v, want ErrNoClass", err)
	}
}

// TestEnginePersistenceViaWAL closes and reopens without an explicit checkpoint;
// recovery must replay the WAL so all acknowledged writes survive.
func TestEnginePersistenceViaWAL(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e.CreateClass("Pessoa")
	e.CreateObject("Pessoa", []byte("alice"))
	e.CreateObject("Pessoa", []byte("bob"))
	e.Close()

	e2 := openEngine(t, dir)
	if !e2.ClassExists("Pessoa") {
		t.Fatalf("class lost across restart")
	}
	got, found, _ := e2.GetObject("Pessoa", 1)
	if !found || string(got) != "alice" {
		t.Fatalf("object 1 lost: (%q,%v)", got, found)
	}
	got, found, _ = e2.GetObject("Pessoa", 2)
	if !found || string(got) != "bob" {
		t.Fatalf("object 2 lost: (%q,%v)", got, found)
	}
	// New ids must continue past the restored maximum.
	id, _ := e2.CreateObject("Pessoa", []byte("carol"))
	if id != 3 {
		t.Fatalf("next id after restart = %d, want 3", id)
	}
}

func TestEngineCreateObjectsBulk(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e.CreateClass("Item")

	datas := make([][]byte, 500)
	for i := range datas {
		datas[i] = []byte(fmt.Sprintf("v%d", i))
	}
	ids, err := e.CreateObjectsBulk("Item", datas)
	if err != nil {
		t.Fatalf("CreateObjectsBulk: %v", err)
	}
	if len(ids) != 500 {
		t.Fatalf("got %d ids, want 500", len(ids))
	}
	// Ids are sequential and unique.
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("ids[%d] = %d, want %d", i, id, i+1)
		}
	}
	// Read-after-write from the overlay.
	got, found, _ := e.GetObject("Item", 250)
	if !found || string(got) != "v249" {
		t.Fatalf("GetObject 250 = (%q,%v), want v249", got, found)
	}
	// Empty bulk is a no-op.
	if ids, err := e.CreateObjectsBulk("Item", nil); err != nil || len(ids) != 0 {
		t.Fatalf("empty bulk = (%v,%v)", ids, err)
	}
	// Bulk on missing class errors.
	if _, err := e.CreateObjectsBulk("Ghost", [][]byte{[]byte("x")}); !errors.Is(err, ErrNoClass) {
		t.Fatalf("bulk missing class = %v, want ErrNoClass", err)
	}

	// Persist across restart: the whole batch is durable via WAL replay.
	e.Close()
	e2, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Close()
	all, _ := e2.ListObjects("Item", 0, 0)
	if len(all) != 500 {
		t.Fatalf("after restart: %d objects, want 500", len(all))
	}
	// Next id continues past the batch.
	if id, _ := e2.CreateObject("Item", []byte("next")); id != 501 {
		t.Fatalf("next id after restart = %d, want 501", id)
	}
}

func TestEngineBulkAtomicViaCheckpoint(t *testing.T) {
	dir := t.TempDir()
	e, _ := Open(Config{Dir: dir})
	e.CreateClass("Item")
	datas := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	e.CreateObjectsBulk("Item", datas)
	// Checkpoint folds the single batch record; all three must survive.
	if err := e.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	e.Close()

	e2 := openEngine(t, dir)
	all, _ := e2.ListObjects("Item", 0, 0)
	if len(all) != 3 {
		t.Fatalf("after checkpoint+restart: %d objects, want 3", len(all))
	}
}

// TestEngineConcurrentWriters exercises many writers on the same and different
// classes at once, checking for deadlock (timeout guard) and correct counts.
func TestEngineConcurrentWriters(t *testing.T) {
	e, err := Open(Config{Dir: t.TempDir(), Fsync: wal.GroupCommitMode(2*time.Millisecond, 256)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()
	e.CreateClass("A")
	e.CreateClass("B")

	const writers = 20
	const per = 50
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			class := "A"
			if i%2 == 0 {
				class = "B"
			}
			for j := 0; j < per; j++ {
				if _, err := e.CreateObject(class, []byte(fmt.Sprintf("w%d-%d", i, j))); err != nil {
					t.Errorf("CreateObject: %v", err)
					return
				}
			}
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock: concurrent writers did not finish in 60s")
	}

	a, _ := e.ListObjects("A", 0, 0)
	b, _ := e.ListObjects("B", 0, 0)
	if len(a)+len(b) != writers*per {
		t.Fatalf("total objects = %d, want %d", len(a)+len(b), writers*per)
	}
}

// TestEnginePersistenceViaCheckpoint closes after a checkpoint and reopens.
func TestEnginePersistenceViaCheckpoint(t *testing.T) {
	dir := t.TempDir()
	e, _ := Open(Config{Dir: dir})
	e.CreateClass("Pessoa")
	e.CreateObject("Pessoa", []byte("alice"))
	e.Checkpoint()
	e.CreateObject("Pessoa", []byte("bob")) // in WAL only, post-checkpoint
	e.Close()

	e2 := openEngine(t, dir)
	got, found, _ := e2.GetObject("Pessoa", 1)
	if !found || string(got) != "alice" {
		t.Fatalf("checkpointed object lost: (%q,%v)", got, found)
	}
	got, found, _ = e2.GetObject("Pessoa", 2)
	if !found || string(got) != "bob" {
		t.Fatalf("post-checkpoint object lost: (%q,%v)", got, found)
	}
}
