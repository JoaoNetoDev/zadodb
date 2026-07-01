// Package btree implements a copy-on-write B+Tree serialized into fixed-size
// pages. Every mutation writes brand-new pages along the path from the changed
// leaf up to a new root, and never overwrites a live page. This is what lets a
// checkpoint publish a new tree with a single atomic rename, and lets readers
// navigate an immutable snapshot over mmap without any locking.
package btree

import (
	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
)

// maxInline is the largest value stored directly inside a leaf entry. Larger
// values spill into an overflow page chain so leaves stay shallow and splits
// stay cheap.
const maxInline = 512

// PageSource reads pages by id (read path: mmap snapshot or data file).
type PageSource interface {
	ReadPage(id page.PageID) (*page.Page, error)
}

// PageStore additionally allocates and writes pages (write path: the data file
// under construction during a checkpoint or in-place replay).
type PageStore interface {
	PageSource
	Allocate() page.PageID
	WritePage(p *page.Page) error
}

// ovRef references an overflow chain holding a large value.
type ovRef struct {
	head page.PageID
	size uint64
}

// leafEntry is one key/value pair in a leaf. The value is either stored inline
// or referenced through an overflow chain.
type leafEntry struct {
	key        []byte
	inline     []byte // valid when !isOverflow
	ov         ovRef  // valid when isOverflow
	isOverflow bool
}

// node is the in-memory form of a B+Tree page. Leaves hold entries; internal
// nodes hold separator keys and child pointers (len(children) == len(keys)+1).
type node struct {
	leaf     bool
	entries  []leafEntry   // leaf only, kept sorted by key
	keys     [][]byte      // internal only, sorted separators
	children []page.PageID // internal only
}

// leafSearch returns the index of key and whether it was found, using a linear
// scan (nodes are small — a few KB).
func (n *node) leafSearch(key []byte) (int, bool) {
	for i, e := range n.entries {
		switch compareBytes(e.key, key) {
		case 0:
			return i, true
		case 1: // e.key > key: insertion point
			return i, false
		}
	}
	return len(n.entries), false
}

// childIndex returns the index of the child to descend into for key.
func (n *node) childIndex(key []byte) int {
	// children[i] covers keys < keys[i]; the last child covers the rest.
	for i, sep := range n.keys {
		if compareBytes(key, sep) < 0 {
			return i
		}
	}
	return len(n.children) - 1
}

// compareBytes returns -1, 0, or 1. Kept local so the hot path avoids the
// bytes package's extra bounds work; identical semantics to bytes.Compare.
func compareBytes(a, b []byte) int {
	la, lb := len(a), len(b)
	n := la
	if lb < n {
		n = lb
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case la < lb:
		return -1
	case la > lb:
		return 1
	default:
		return 0
	}
}
