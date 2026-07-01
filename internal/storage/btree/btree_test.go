package btree

import (
	"bytes"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
)

func newStore(t *testing.T) *page.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tree.zdb")
	m, err := page.Create(path)
	if err != nil {
		t.Fatalf("Create store: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func key(i int) []byte { return []byte(fmt.Sprintf("key-%08d", i)) }
func val(i int) []byte { return []byte(fmt.Sprintf("value-for-%d", i)) }

func TestInsertGetManyRandom(t *testing.T) {
	store := newStore(t)
	tree := NewEmpty(store)

	const n = 5000
	order := rand.New(rand.NewSource(1)).Perm(n)
	for _, i := range order {
		if err := tree.Insert(key(i), val(i)); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		got, found, err := tree.Get(key(i))
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if !found {
			t.Fatalf("key %d missing", i)
		}
		if !bytes.Equal(got, val(i)) {
			t.Fatalf("key %d = %q, want %q", i, got, val(i))
		}
	}
	// A key that was never inserted must be absent.
	if _, found, _ := tree.Get(key(n + 1)); found {
		t.Fatalf("unexpected key found")
	}
}

func TestReplaceValue(t *testing.T) {
	store := newStore(t)
	tree := NewEmpty(store)
	tree.Insert([]byte("k"), []byte("v1"))
	tree.Insert([]byte("k"), []byte("v2"))
	got, found, _ := tree.Get([]byte("k"))
	if !found || string(got) != "v2" {
		t.Fatalf("after replace = (%q,%v), want (v2,true)", got, found)
	}
}

func TestDelete(t *testing.T) {
	store := newStore(t)
	tree := NewEmpty(store)
	const n = 2000
	for i := 0; i < n; i++ {
		tree.Insert(key(i), val(i))
	}
	// Delete evens.
	for i := 0; i < n; i += 2 {
		if err := tree.Delete(key(i)); err != nil {
			t.Fatalf("Delete %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		_, found, _ := tree.Get(key(i))
		wantFound := i%2 == 1
		if found != wantFound {
			t.Fatalf("key %d found=%v, want %v", i, found, wantFound)
		}
	}
	// Deleting a missing key is a harmless no-op.
	if err := tree.Delete(key(999999)); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestCopyOnWriteImmutability(t *testing.T) {
	store := newStore(t)
	tree := NewEmpty(store)

	// Build an initial state and capture its root.
	for i := 0; i < 1000; i++ {
		tree.Insert(key(i), val(i))
	}
	oldRoot := tree.Root()

	// Mutate: add new keys and delete some old ones. This advances the root.
	for i := 1000; i < 2000; i++ {
		tree.Insert(key(i), val(i))
	}
	for i := 0; i < 500; i++ {
		tree.Delete(key(i))
	}
	if tree.Root() == oldRoot {
		t.Fatal("root did not advance after mutation")
	}

	// The OLD snapshot must still reflect the original state exactly: all
	// original keys present, none of the new keys visible. This proves no live
	// page from the old tree was overwritten.
	for i := 0; i < 1000; i++ {
		got, found, err := Get(store, oldRoot, key(i))
		if err != nil || !found || !bytes.Equal(got, val(i)) {
			t.Fatalf("old snapshot key %d = (%q,%v,%v), want original", i, got, found, err)
		}
	}
	for i := 1000; i < 2000; i++ {
		if _, found, _ := Get(store, oldRoot, key(i)); found {
			t.Fatalf("old snapshot should not see new key %d", i)
		}
	}
}

func TestOverflowLargeValue(t *testing.T) {
	store := newStore(t)
	tree := NewEmpty(store)

	// A value spanning several overflow pages (> PageSize).
	big := make([]byte, 20000)
	for i := range big {
		big[i] = byte(i * 31)
	}
	if err := tree.Insert([]byte("big"), big); err != nil {
		t.Fatalf("Insert big: %v", err)
	}
	// A few small neighbours to ensure the leaf stays consistent.
	tree.Insert([]byte("a"), []byte("1"))
	tree.Insert([]byte("z"), []byte("2"))

	got, found, err := tree.Get([]byte("big"))
	if err != nil || !found {
		t.Fatalf("Get big = found %v err %v", found, err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("big value round-trip mismatch (len got %d want %d)", len(got), len(big))
	}

	// Replace the big value with a small one, then read back.
	tree.Insert([]byte("big"), []byte("small now"))
	got, _, _ = tree.Get([]byte("big"))
	if string(got) != "small now" {
		t.Fatalf("after shrink = %q, want 'small now'", got)
	}
}

func TestScanPrefix(t *testing.T) {
	store := newStore(t)
	tree := NewEmpty(store)

	// Two prefixes interleaved.
	for i := 0; i < 300; i++ {
		tree.Insert([]byte(fmt.Sprintf("A/%05d", i)), []byte("a"))
		tree.Insert([]byte(fmt.Sprintf("B/%05d", i)), []byte("b"))
	}

	var got []string
	err := tree.Scan([]byte("A/"), func(k, v []byte) bool {
		if string(v) != "a" {
			t.Errorf("scan returned B value under A prefix")
		}
		got = append(got, string(k))
		return true
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 300 {
		t.Fatalf("scanned %d keys, want 300", len(got))
	}
	// Results must be ascending.
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("scan not ascending at %d: %q >= %q", i, got[i-1], got[i])
		}
	}

	// Early stop via fn returning false.
	count := 0
	tree.Scan([]byte("B/"), func(k, v []byte) bool {
		count++
		return count < 10
	})
	if count != 10 {
		t.Fatalf("early-stop scan visited %d, want 10", count)
	}
}

func TestMultiLevelTree(t *testing.T) {
	// Insert enough entries with sizable values to force several levels of
	// internal nodes (cascading splits).
	store := newStore(t)
	tree := NewEmpty(store)
	const n = 8000
	filler := bytes.Repeat([]byte("x"), 200)
	for i := 0; i < n; i++ {
		v := append([]byte(fmt.Sprintf("%d:", i)), filler...)
		if err := tree.Insert(key(i), v); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	// Verify the root is an internal node (tree grew past one leaf).
	rootPage, _ := store.ReadPage(tree.Root())
	if rootPage.Type() != page.PageTypeBTreeInternal {
		t.Fatalf("root type = %s, want internal (tree did not grow)", rootPage.Type())
	}
	// Spot-check retrieval.
	for _, i := range []int{0, 1, n / 2, n - 1} {
		got, found, err := tree.Get(key(i))
		if err != nil || !found {
			t.Fatalf("Get %d: found %v err %v", i, found, err)
		}
		if !bytes.HasPrefix(got, []byte(fmt.Sprintf("%d:", i))) {
			t.Fatalf("Get %d prefix mismatch", i)
		}
	}
}
