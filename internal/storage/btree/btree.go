package btree

import "github.com/JoaoNetoDev/zadodb/internal/storage/page"

// Tree is a copy-on-write B+Tree rooted at a page id. Insert and Delete never
// mutate existing pages; they write new pages and advance the root, so any
// previously-captured root remains a valid immutable snapshot.
type Tree struct {
	store PageStore
	root  page.PageID
}

// NewEmpty returns an empty tree (no root page yet).
func NewEmpty(store PageStore) *Tree {
	return &Tree{store: store, root: page.InvalidPageID}
}

// Load returns a tree over an existing root.
func Load(store PageStore, root page.PageID) *Tree {
	return &Tree{store: store, root: root}
}

// Root returns the current root page id (InvalidPageID when empty).
func (t *Tree) Root() page.PageID { return t.root }

// Get looks up key in the tree.
func (t *Tree) Get(key []byte) ([]byte, bool, error) {
	return Get(t.store, t.root, key)
}

// Scan visits every key/value whose key has the given prefix, in ascending
// order, until fn returns false.
func (t *Tree) Scan(prefix []byte, fn func(key, value []byte) bool) error {
	return Scan(t.store, t.root, prefix, fn)
}

// Insert adds or replaces key with value, advancing the root.
func (t *Tree) Insert(key, value []byte) error {
	ve, err := t.makeValue(key, value)
	if err != nil {
		return err
	}
	if t.root == page.InvalidPageID {
		leaf := &node{leaf: true, entries: []leafEntry{ve}}
		id := t.store.Allocate()
		if err := t.store.WritePage(leaf.encodeTo(id)); err != nil {
			return err
		}
		t.root = id
		return nil
	}
	newID, split, err := t.insert(t.root, ve)
	if err != nil {
		return err
	}
	if split != nil {
		root := &node{
			leaf:     false,
			keys:     [][]byte{split.sep},
			children: []page.PageID{newID, split.right},
		}
		id := t.store.Allocate()
		if err := t.store.WritePage(root.encodeTo(id)); err != nil {
			return err
		}
		t.root = id
		return nil
	}
	t.root = newID
	return nil
}

// Delete removes key if present, advancing the root. Missing keys are a no-op.
// Nodes are not merged/rebalanced (space is reclaimed at the next checkpoint);
// this keeps deletes simple and lookups always correct.
func (t *Tree) Delete(key []byte) error {
	if t.root == page.InvalidPageID {
		return nil
	}
	newID, err := t.delete(t.root, key)
	if err != nil {
		return err
	}
	t.root = newID
	return nil
}

// makeValue builds a leaf entry, spilling large values into an overflow chain.
func (t *Tree) makeValue(key, value []byte) (leafEntry, error) {
	return makeLeafEntry(t.store, key, value)
}

// makeLeafEntry builds a leaf entry over any store, spilling large values into
// an overflow chain. Shared by the COW insert path and the bulk builder.
func makeLeafEntry(store PageStore, key, value []byte) (leafEntry, error) {
	e := leafEntry{key: append([]byte(nil), key...)}
	if len(value) <= maxInline {
		e.inline = append([]byte(nil), value...)
		return e, nil
	}
	ref, err := writeOverflow(store, value)
	if err != nil {
		return leafEntry{}, err
	}
	e.isOverflow = true
	e.ov = ref
	return e, nil
}

// entrySize returns the serialized byte cost of a leaf entry (matching
// encodedSize's per-entry accounting).
func (e leafEntry) size() int {
	if e.isOverflow {
		return 2 + len(e.key) + 1 + 16
	}
	return 2 + len(e.key) + 1 + 4 + len(e.inline)
}

func (t *Tree) insert(id page.PageID, ve leafEntry) (page.PageID, *splitResult, error) {
	p, err := t.store.ReadPage(id)
	if err != nil {
		return 0, nil, err
	}
	n, err := decodeNode(p)
	if err != nil {
		return 0, nil, err
	}

	if n.leaf {
		idx, found := n.leafSearch(ve.key)
		entries := make([]leafEntry, 0, len(n.entries)+1)
		entries = append(entries, n.entries[:idx]...)
		entries = append(entries, ve)
		if found {
			entries = append(entries, n.entries[idx+1:]...)
		} else {
			entries = append(entries, n.entries[idx:]...)
		}
		nn := &node{leaf: true, entries: entries}
		return t.writeMaybeSplit(nn)
	}

	ci := n.childIndex(ve.key)
	childNewID, childSplit, err := t.insert(n.children[ci], ve)
	if err != nil {
		return 0, nil, err
	}
	keys := append([][]byte(nil), n.keys...)
	children := append([]page.PageID(nil), n.children...)
	children[ci] = childNewID
	if childSplit != nil {
		keys = insertKeyAt(keys, ci, childSplit.sep)
		children = insertChildAt(children, ci+1, childSplit.right)
	}
	nn := &node{leaf: false, keys: keys, children: children}
	return t.writeMaybeSplit(nn)
}

// writeMaybeSplit persists a node, splitting it first if it no longer fits.
func (t *Tree) writeMaybeSplit(n *node) (page.PageID, *splitResult, error) {
	if n.fits() {
		id := t.store.Allocate()
		if err := t.store.WritePage(n.encodeTo(id)); err != nil {
			return 0, nil, err
		}
		return id, nil, nil
	}
	var left, right *node
	var sep []byte
	if n.leaf {
		left, right, sep = splitLeaf(n)
	} else {
		left, right, sep = splitInternal(n)
	}
	lid := t.store.Allocate()
	if err := t.store.WritePage(left.encodeTo(lid)); err != nil {
		return 0, nil, err
	}
	rid := t.store.Allocate()
	if err := t.store.WritePage(right.encodeTo(rid)); err != nil {
		return 0, nil, err
	}
	return lid, &splitResult{sep: sep, right: rid}, nil
}

func (t *Tree) delete(id page.PageID, key []byte) (page.PageID, error) {
	p, err := t.store.ReadPage(id)
	if err != nil {
		return 0, err
	}
	n, err := decodeNode(p)
	if err != nil {
		return 0, err
	}

	if n.leaf {
		idx, found := n.leafSearch(key)
		if !found {
			return id, nil // unchanged: no new page
		}
		entries := make([]leafEntry, 0, len(n.entries)-1)
		entries = append(entries, n.entries[:idx]...)
		entries = append(entries, n.entries[idx+1:]...)
		nn := &node{leaf: true, entries: entries}
		nid := t.store.Allocate()
		if err := t.store.WritePage(nn.encodeTo(nid)); err != nil {
			return 0, err
		}
		return nid, nil
	}

	ci := n.childIndex(key)
	childNewID, err := t.delete(n.children[ci], key)
	if err != nil {
		return 0, err
	}
	if childNewID == n.children[ci] {
		return id, nil // unchanged
	}
	children := append([]page.PageID(nil), n.children...)
	children[ci] = childNewID
	nn := &node{leaf: false, keys: append([][]byte(nil), n.keys...), children: children}
	nid := t.store.Allocate()
	if err := t.store.WritePage(nn.encodeTo(nid)); err != nil {
		return 0, err
	}
	return nid, nil
}

// Get looks up key starting from root over any page source (read-only).
func Get(src PageSource, root page.PageID, key []byte) ([]byte, bool, error) {
	if root == page.InvalidPageID {
		return nil, false, nil
	}
	id := root
	for {
		p, err := src.ReadPage(id)
		if err != nil {
			return nil, false, err
		}
		n, err := decodeNode(p)
		if err != nil {
			return nil, false, err
		}
		if n.leaf {
			idx, found := n.leafSearch(key)
			if !found {
				return nil, false, nil
			}
			v, err := n.entries[idx].materialize(src)
			if err != nil {
				return nil, false, err
			}
			return v, true, nil
		}
		id = n.children[n.childIndex(key)]
	}
}

// Scan visits every key/value with the given prefix in ascending order until fn
// returns false, over any read-only page source.
func Scan(src PageSource, root page.PageID, prefix []byte, fn func(key, value []byte) bool) error {
	if root == page.InvalidPageID {
		return nil
	}
	lo := prefix
	hi := prefixUpper(prefix)
	stopped := false

	var walk func(id page.PageID) error
	walk = func(id page.PageID) error {
		if stopped {
			return nil
		}
		p, err := src.ReadPage(id)
		if err != nil {
			return err
		}
		n, err := decodeNode(p)
		if err != nil {
			return err
		}
		if n.leaf {
			for _, e := range n.entries {
				if stopped {
					return nil
				}
				if len(lo) > 0 && compareBytes(e.key, lo) < 0 {
					continue
				}
				if hi != nil && compareBytes(e.key, hi) >= 0 {
					continue
				}
				v, err := e.materialize(src)
				if err != nil {
					return err
				}
				if !fn(e.key, v) {
					stopped = true
					return nil
				}
			}
			return nil
		}
		for i := 0; i < len(n.children); i++ {
			if stopped {
				return nil
			}
			// child i holds keys in [keys[i-1], keys[i]); prune non-overlap.
			if i < len(n.keys) && len(lo) > 0 && compareBytes(n.keys[i], lo) <= 0 {
				continue
			}
			if i > 0 && hi != nil && compareBytes(n.keys[i-1], hi) >= 0 {
				continue
			}
			if err := walk(n.children[i]); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root)
}
