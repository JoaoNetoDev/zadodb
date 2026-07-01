package mvcc

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/JoaoNetoDev/zadodb/internal/storage/btree"
	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
)

// buildData writes a data file containing the given key/value pairs and a meta
// page pointing at the tree root.
func buildData(t *testing.T, path string, kv map[string]string) {
	t.Helper()
	m, err := page.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tree := btree.NewEmpty(m)
	for k, v := range kv {
		if err := tree.Insert([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if err := m.WriteMeta(page.Meta{Root: tree.Root(), NumPages: m.NumPages()}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	if err := m.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	m.Close()
}

func TestSnapshotGetAndScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.000000.zdb")
	kv := map[string]string{}
	for i := 0; i < 200; i++ {
		kv[fmt.Sprintf("obj/%03d", i)] = fmt.Sprintf("v%d", i)
	}
	buildData(t, path, kv)

	mf, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mf.Close()

	snap := mf.Acquire()
	defer snap.Release()

	for k, want := range kv {
		got, found, err := snap.Get([]byte(k))
		if err != nil || !found {
			t.Fatalf("Get %s: found %v err %v", k, found, err)
		}
		if string(got) != want {
			t.Fatalf("Get %s = %q, want %q", k, got, want)
		}
	}

	count := 0
	if err := snap.Scan([]byte("obj/"), func(k, v []byte) bool {
		count++
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if count != len(kv) {
		t.Fatalf("scanned %d, want %d", count, len(kv))
	}
}

func TestConcurrentReadsDuringSwap(t *testing.T) {
	dir := t.TempDir()
	gen0 := filepath.Join(dir, "data.000000.zdb")
	gen1 := filepath.Join(dir, "data.000001.zdb")
	buildData(t, gen0, map[string]string{"k": "gen0", "shared": "x"})
	buildData(t, gen1, map[string]string{"k": "gen1", "shared": "x"})

	mf, err := Open(gen0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mf.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Many concurrent readers.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := mf.Acquire()
				got, found, err := snap.Get([]byte("k"))
				if err != nil || !found {
					t.Errorf("reader Get: found %v err %v", found, err)
				}
				if s := string(got); s != "gen0" && s != "gen1" {
					t.Errorf("reader got unexpected value %q", s)
				}
				snap.Release()
			}
		}()
	}

	// Swapper flips generations repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		paths := []string{gen0, gen1}
		for i := 0; i < 500; i++ {
			if err := mf.SwapTo(paths[i%2]); err != nil {
				t.Errorf("SwapTo: %v", err)
				return
			}
		}
		close(stop)
	}()

	wg.Wait()
}
