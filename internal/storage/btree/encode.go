package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
)

// Leaf entry flags.
const (
	flagInline   = 0
	flagOverflow = 1
)

// encodedSize returns how many body bytes the node needs.
func (n *node) encodedSize() int {
	if n.leaf {
		sz := 2 // numKeys
		for _, e := range n.entries {
			sz += 2 + len(e.key) + 1 // keyLen + key + flag
			if e.isOverflow {
				sz += 16 // head(8) + size(8)
			} else {
				sz += 4 + len(e.inline) // valLen + val
			}
		}
		return sz
	}
	sz := 2 + 8 // numKeys + child0
	for _, k := range n.keys {
		sz += 2 + len(k) + 8 // keyLen + key + childNext
	}
	return sz
}

// fits reports whether the node serializes within a page body.
func (n *node) fits() bool { return n.encodedSize() <= page.BodySize }

// encodeTo serializes the node into a fresh page with the given id.
func (n *node) encodeTo(id page.PageID) *page.Page {
	var p *page.Page
	body := make([]byte, 0, n.encodedSize())
	if n.leaf {
		p = page.New(id, page.PageTypeBTreeLeaf)
		body = binary.LittleEndian.AppendUint16(body, uint16(len(n.entries)))
		for _, e := range n.entries {
			body = binary.LittleEndian.AppendUint16(body, uint16(len(e.key)))
			body = append(body, e.key...)
			if e.isOverflow {
				body = append(body, flagOverflow)
				body = binary.LittleEndian.AppendUint64(body, uint64(e.ov.head))
				body = binary.LittleEndian.AppendUint64(body, e.ov.size)
			} else {
				body = append(body, flagInline)
				body = binary.LittleEndian.AppendUint32(body, uint32(len(e.inline)))
				body = append(body, e.inline...)
			}
		}
	} else {
		p = page.New(id, page.PageTypeBTreeInternal)
		body = binary.LittleEndian.AppendUint16(body, uint16(len(n.keys)))
		body = binary.LittleEndian.AppendUint64(body, uint64(n.children[0]))
		for i, k := range n.keys {
			body = binary.LittleEndian.AppendUint16(body, uint16(len(k)))
			body = append(body, k...)
			body = binary.LittleEndian.AppendUint64(body, uint64(n.children[i+1]))
		}
	}
	copy(p.Body(), body)
	p.SetPayloadLen(uint32(len(body)))
	return p
}

// decodeNode parses a page into an in-memory node.
func decodeNode(p *page.Page) (*node, error) {
	body := p.Payload()
	switch p.Type() {
	case page.PageTypeBTreeLeaf:
		return decodeLeaf(body)
	case page.PageTypeBTreeInternal:
		return decodeInternal(body)
	default:
		return nil, fmt.Errorf("btree: page %d is not a btree node (type %s)", p.ID(), p.Type())
	}
}

func decodeLeaf(body []byte) (*node, error) {
	n := &node{leaf: true}
	if len(body) < 2 {
		return nil, fmt.Errorf("btree: truncated leaf")
	}
	num := int(binary.LittleEndian.Uint16(body))
	off := 2
	n.entries = make([]leafEntry, 0, num)
	for i := 0; i < num; i++ {
		if off+2 > len(body) {
			return nil, fmt.Errorf("btree: truncated leaf key len")
		}
		kl := int(binary.LittleEndian.Uint16(body[off:]))
		off += 2
		if off+kl+1 > len(body) {
			return nil, fmt.Errorf("btree: truncated leaf key")
		}
		key := append([]byte(nil), body[off:off+kl]...)
		off += kl
		flag := body[off]
		off++
		var e leafEntry
		e.key = key
		if flag == flagOverflow {
			if off+16 > len(body) {
				return nil, fmt.Errorf("btree: truncated overflow ref")
			}
			e.isOverflow = true
			e.ov.head = page.PageID(binary.LittleEndian.Uint64(body[off:]))
			e.ov.size = binary.LittleEndian.Uint64(body[off+8:])
			off += 16
		} else {
			if off+4 > len(body) {
				return nil, fmt.Errorf("btree: truncated value len")
			}
			vl := int(binary.LittleEndian.Uint32(body[off:]))
			off += 4
			if off+vl > len(body) {
				return nil, fmt.Errorf("btree: truncated value")
			}
			e.inline = append([]byte(nil), body[off:off+vl]...)
			off += vl
		}
		n.entries = append(n.entries, e)
	}
	return n, nil
}

func decodeInternal(body []byte) (*node, error) {
	n := &node{leaf: false}
	if len(body) < 10 {
		return nil, fmt.Errorf("btree: truncated internal node")
	}
	num := int(binary.LittleEndian.Uint16(body))
	off := 2
	n.children = make([]page.PageID, 0, num+1)
	n.keys = make([][]byte, 0, num)
	n.children = append(n.children, page.PageID(binary.LittleEndian.Uint64(body[off:])))
	off += 8
	for i := 0; i < num; i++ {
		if off+2 > len(body) {
			return nil, fmt.Errorf("btree: truncated separator len")
		}
		kl := int(binary.LittleEndian.Uint16(body[off:]))
		off += 2
		if off+kl+8 > len(body) {
			return nil, fmt.Errorf("btree: truncated separator")
		}
		n.keys = append(n.keys, append([]byte(nil), body[off:off+kl]...))
		off += kl
		n.children = append(n.children, page.PageID(binary.LittleEndian.Uint64(body[off:])))
		off += 8
	}
	return n, nil
}

// overflowHeader is the per-page overflow prelude: next page id + chunk length.
const overflowHeader = 12 // u64 next + u32 chunkLen

// writeOverflow stores val across a fresh chain of overflow pages and returns
// the chain head and total size. All pages are newly allocated (COW).
func writeOverflow(store PageStore, val []byte) (ovRef, error) {
	chunk := page.BodySize - overflowHeader
	// Build the chain back-to-front so each page can point at its successor.
	type built struct {
		id   page.PageID
		data []byte
		next page.PageID
	}
	var pages []built
	for off := 0; off < len(val); off += chunk {
		end := off + chunk
		if end > len(val) {
			end = len(val)
		}
		pages = append(pages, built{id: store.Allocate(), data: val[off:end]})
	}
	if len(pages) == 0 { // empty value still needs one page
		pages = append(pages, built{id: store.Allocate(), data: nil})
	}
	for i := range pages {
		if i+1 < len(pages) {
			pages[i].next = pages[i+1].id
		} else {
			pages[i].next = page.InvalidPageID
		}
	}
	for _, b := range pages {
		p := page.New(b.id, page.PageTypeOverflow)
		body := p.Body()
		binary.LittleEndian.PutUint64(body[0:], uint64(b.next))
		binary.LittleEndian.PutUint32(body[8:], uint32(len(b.data)))
		copy(body[overflowHeader:], b.data)
		p.SetPayloadLen(uint32(overflowHeader + len(b.data)))
		if err := store.WritePage(p); err != nil {
			return ovRef{}, err
		}
	}
	return ovRef{head: pages[0].id, size: uint64(len(val))}, nil
}

// readOverflow reassembles a value from its overflow chain.
func readOverflow(src PageSource, ref ovRef) ([]byte, error) {
	out := make([]byte, 0, ref.size)
	id := ref.head
	for id != page.InvalidPageID {
		p, err := src.ReadPage(id)
		if err != nil {
			return nil, err
		}
		if p.Type() != page.PageTypeOverflow {
			return nil, fmt.Errorf("btree: page %d is not an overflow page", id)
		}
		body := p.Payload()
		next := page.PageID(binary.LittleEndian.Uint64(body[0:]))
		clen := int(binary.LittleEndian.Uint32(body[8:]))
		if overflowHeader+clen > len(body) {
			return nil, fmt.Errorf("btree: corrupt overflow page %d", id)
		}
		out = append(out, body[overflowHeader:overflowHeader+clen]...)
		id = next
	}
	return out, nil
}

// materialize returns the concrete value of a leaf entry, reading overflow if
// needed.
func (e leafEntry) materialize(src PageSource) ([]byte, error) {
	if e.isOverflow {
		return readOverflow(src, e.ov)
	}
	return append([]byte(nil), e.inline...), nil
}
