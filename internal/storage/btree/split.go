package btree

import "github.com/JoaoNetoDev/zadodb/internal/storage/page"

// splitResult describes a node that split during insertion: the separator key
// promoted to the parent and the page id of the new right sibling.
type splitResult struct {
	sep   []byte
	right page.PageID
}

// splitLeaf divides a leaf's entries in half. The B+Tree separator promoted to
// the parent is a copy of the right half's first key.
//
// A single leaf entry is bounded (key + at most maxInline value, or a 16-byte
// overflow ref), so a half-split of an over-full leaf always yields two halves
// that fit within a page.
func splitLeaf(n *node) (left, right *node, sep []byte) {
	mid := len(n.entries) / 2
	if mid == 0 {
		mid = 1
	}
	left = &node{leaf: true, entries: append([]leafEntry(nil), n.entries[:mid]...)}
	right = &node{leaf: true, entries: append([]leafEntry(nil), n.entries[mid:]...)}
	sep = append([]byte(nil), right.entries[0].key...)
	return left, right, sep
}

// splitInternal divides an internal node. Unlike a leaf split, the middle
// separator moves up to the parent (it is not duplicated).
func splitInternal(n *node) (left, right *node, sep []byte) {
	mid := len(n.keys) / 2
	sep = append([]byte(nil), n.keys[mid]...)
	left = &node{
		leaf:     false,
		keys:     append([][]byte(nil), n.keys[:mid]...),
		children: append([]page.PageID(nil), n.children[:mid+1]...),
	}
	right = &node{
		leaf:     false,
		keys:     append([][]byte(nil), n.keys[mid+1:]...),
		children: append([]page.PageID(nil), n.children[mid+1:]...),
	}
	return left, right, sep
}

// insertKeyAt returns keys with k inserted at index i.
func insertKeyAt(keys [][]byte, i int, k []byte) [][]byte {
	keys = append(keys, nil)
	copy(keys[i+1:], keys[i:])
	keys[i] = k
	return keys
}

// insertChildAt returns children with c inserted at index i.
func insertChildAt(children []page.PageID, i int, c page.PageID) []page.PageID {
	children = append(children, 0)
	copy(children[i+1:], children[i:])
	children[i] = c
	return children
}

// prefixUpper returns the smallest key strictly greater than every key sharing
// prefix, or nil for an unbounded upper end (prefix empty or all 0xFF).
func prefixUpper(prefix []byte) []byte {
	up := append([]byte(nil), prefix...)
	for i := len(up) - 1; i >= 0; i-- {
		if up[i] != 0xFF {
			up[i]++
			return up[:i+1]
		}
	}
	return nil
}
