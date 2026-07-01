package btree

import (
	"fmt"

	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
)

// Builder bulk-loads a compact B+Tree from keys supplied in strictly ascending
// order. Leaves are packed nearly full and internal levels are built bottom-up,
// so the result has no copy-on-write orphans — its size is proportional to the
// live data, not to the write history. This is what makes a compacting
// checkpoint bounded (vs. incremental COW inserts, which amplify wildly).
//
// Memory is bounded: only the current leaf and the per-level index of
// (firstKey, pageID) are held, not the whole dataset.
type Builder struct {
	store     PageStore
	leaf      []leafEntry
	leafBytes int
	leaves    []levelEntry
	lastKey   []byte
	count     int
}

type levelEntry struct {
	firstKey []byte
	page     page.PageID
}

// NewBuilder returns a builder writing into store.
func NewBuilder(store PageStore) *Builder {
	return &Builder{store: store, leafBytes: 2} // 2 = leaf numKeys header
}

// Count returns how many entries have been added.
func (b *Builder) Count() int { return b.count }

// Add appends one key/value. Keys must be strictly ascending.
func (b *Builder) Add(key, value []byte) error {
	if b.lastKey != nil && compareBytes(key, b.lastKey) <= 0 {
		return fmt.Errorf("btree: Builder.Add: keys not strictly ascending")
	}
	e, err := makeLeafEntry(b.store, key, value)
	if err != nil {
		return err
	}
	es := e.size()
	if len(b.leaf) > 0 && b.leafBytes+es > page.BodySize {
		if err := b.flushLeaf(); err != nil {
			return err
		}
	}
	b.leaf = append(b.leaf, e)
	b.leafBytes += es
	b.lastKey = append([]byte(nil), key...)
	b.count++
	return nil
}

func (b *Builder) flushLeaf() error {
	if len(b.leaf) == 0 {
		return nil
	}
	id := b.store.Allocate()
	n := &node{leaf: true, entries: b.leaf}
	if err := b.store.WritePage(n.encodeTo(id)); err != nil {
		return err
	}
	b.leaves = append(b.leaves, levelEntry{
		firstKey: append([]byte(nil), b.leaf[0].key...),
		page:     id,
	})
	b.leaf = nil
	b.leafBytes = 2
	return nil
}

// Finish flushes the last leaf, builds the internal levels, and returns the
// root page id (InvalidPageID for an empty tree).
func (b *Builder) Finish() (page.PageID, error) {
	if err := b.flushLeaf(); err != nil {
		return 0, err
	}
	if len(b.leaves) == 0 {
		return page.InvalidPageID, nil
	}
	level := b.leaves
	for len(level) > 1 {
		next, err := b.buildInternalLevel(level)
		if err != nil {
			return 0, err
		}
		level = next
	}
	return level[0].page, nil
}

// buildInternalLevel packs a list of children into internal nodes, returning
// the (firstKey, pageID) list of the level above.
func (b *Builder) buildInternalLevel(children []levelEntry) ([]levelEntry, error) {
	var out []levelEntry
	i := 0
	for i < len(children) {
		n := &node{leaf: false}
		n.children = append(n.children, children[i].page)
		firstKey := children[i].firstKey
		size := 2 + 8 // numKeys + child0
		j := i + 1
		for j < len(children) {
			k := children[j].firstKey
			add := 2 + len(k) + 8
			if len(n.children) >= 2 && size+add > page.BodySize {
				break
			}
			n.keys = append(n.keys, k)
			n.children = append(n.children, children[j].page)
			size += add
			j++
		}
		id := b.store.Allocate()
		if err := b.store.WritePage(n.encodeTo(id)); err != nil {
			return nil, err
		}
		out = append(out, levelEntry{firstKey: firstKey, page: id})
		i = j
	}
	return out, nil
}
