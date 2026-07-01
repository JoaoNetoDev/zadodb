package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
)

func TestBuilderRoundTrip(t *testing.T) {
	store := newStore(t)
	b := NewBuilder(store)

	const n = 5000
	for i := 0; i < n; i++ {
		if err := b.Add(key(i), val(i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	root, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if root == page.InvalidPageID {
		t.Fatal("root is invalid for non-empty build")
	}

	// Every key is retrievable in the built tree.
	for i := 0; i < n; i++ {
		got, found, err := Get(store, root, key(i))
		if err != nil || !found {
			t.Fatalf("Get %d: found %v err %v", i, found, err)
		}
		if !bytes.Equal(got, val(i)) {
			t.Fatalf("Get %d = %q, want %q", i, got, val(i))
		}
	}
	// Scan yields all in ascending order.
	count := 0
	var prev []byte
	Scan(store, root, nil, func(k, v []byte) bool {
		if prev != nil && compareBytes(prev, k) >= 0 {
			t.Fatalf("scan not ascending at %d", count)
		}
		prev = append([]byte(nil), k...)
		count++
		return true
	})
	if count != n {
		t.Fatalf("scanned %d, want %d", count, n)
	}
}

func TestBuilderEmpty(t *testing.T) {
	store := newStore(t)
	root, err := NewBuilder(store).Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if root != page.InvalidPageID {
		t.Fatalf("empty build root = %d, want InvalidPageID", root)
	}
}

func TestBuilderRejectsUnsorted(t *testing.T) {
	store := newStore(t)
	b := NewBuilder(store)
	b.Add([]byte("b"), []byte("1"))
	if err := b.Add([]byte("a"), []byte("2")); err == nil {
		t.Fatal("expected error for descending key")
	}
	if err := b.Add([]byte("b"), []byte("3")); err == nil {
		t.Fatal("expected error for duplicate key")
	}
}

func TestBuilderOverflowValues(t *testing.T) {
	store := newStore(t)
	b := NewBuilder(store)
	big := bytes.Repeat([]byte("x"), 20000)
	b.Add([]byte("a"), []byte("small"))
	b.Add([]byte("b"), big)
	b.Add([]byte("c"), []byte("small2"))
	root, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, found, err := Get(store, root, []byte("b"))
	if err != nil || !found || !bytes.Equal(got, big) {
		t.Fatalf("overflow value round-trip failed: found %v err %v len %d", found, err, len(got))
	}
}

// TestBuilderIsCompact proves the headline property: a bulk build produces far
// fewer pages than the equivalent copy-on-write insert sequence, because it has
// no orphans.
func TestBuilderIsCompact(t *testing.T) {
	const n = 4000
	filler := bytes.Repeat([]byte("y"), 100)

	// COW inserts one by one.
	cowStore := newStore(t)
	cow := NewEmpty(cowStore)
	for i := 0; i < n; i++ {
		cow.Insert(key(i), append([]byte(fmt.Sprintf("%d:", i)), filler...))
	}
	cowPages := cowStore.NumPages()

	// Bulk build the same data.
	buildStore := newStore(t)
	b := NewBuilder(buildStore)
	for i := 0; i < n; i++ {
		b.Add(key(i), append([]byte(fmt.Sprintf("%d:", i)), filler...))
	}
	b.Finish()
	buildPages := buildStore.NumPages()

	if buildPages >= cowPages {
		t.Fatalf("bulk build (%d pages) should use far fewer pages than COW (%d)", buildPages, cowPages)
	}
	t.Logf("compaction: COW used %d pages, bulk build used %d (%.1fx smaller)",
		cowPages, buildPages, float64(cowPages)/float64(buildPages))
}
