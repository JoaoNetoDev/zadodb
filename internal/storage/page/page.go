// Package page defines ZadoDB's fixed-size on-disk page format and a page
// manager for the data file.
//
// Every page is exactly PageSize bytes so that a page maps 1:1 onto an OS page
// and can be addressed by a simple offset (PageID * PageSize). Each page starts
// with a fixed header carrying a CRC32C checksum over the rest of the page, so
// any torn or bit-flipped page is detected on read.
//
// The golden rule of the engine (see CLAUDE.md) is enforced structurally here:
// the manager only ever appends brand-new pages during a checkpoint/replay and
// never rewrites a live page in place.
package page

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	// PageSize is the fixed size of every page, chosen to match the common OS
	// page size for efficient mmap-backed reads.
	PageSize = 4096
	// HeaderSize is the fixed per-page header length.
	HeaderSize = 32
	// BodySize is the usable payload area after the header.
	BodySize = PageSize - HeaderSize

	// magic identifies a ZadoDB page ("ZDB1" in ASCII, big-endian).
	magic = 0x5A444231
)

// PageID addresses a page within the data file. Page 0 is always the meta page.
type PageID uint64

// InvalidPageID is the sentinel for "no page" (e.g. an absent B+Tree child).
const InvalidPageID PageID = ^PageID(0)

// MetaPageID is the reserved location of the file's meta page.
const MetaPageID PageID = 0

// PageType classifies the contents of a page's body.
type PageType uint8

const (
	PageTypeFree PageType = iota
	PageTypeMeta
	PageTypeBTreeLeaf
	PageTypeBTreeInternal
	PageTypeOverflow
)

func (t PageType) String() string {
	switch t {
	case PageTypeFree:
		return "free"
	case PageTypeMeta:
		return "meta"
	case PageTypeBTreeLeaf:
		return "btree-leaf"
	case PageTypeBTreeInternal:
		return "btree-internal"
	case PageTypeOverflow:
		return "overflow"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// castagnoli is the CRC32C table (hardware-accelerated on modern CPUs).
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Header field offsets within the 32-byte page header.
//
// The checksum lives first and covers bytes [4:PageSize], i.e. everything
// except the checksum field itself. This way any change to magic, type,
// payload length, id, lsn, or body is caught.
const (
	offChecksum   = 0  // uint32
	offMagic      = 4  // uint32
	offType       = 8  // uint8
	offFlags      = 9  // uint8
	_offReserved  = 10 // uint16 (reserved)
	offPayloadLen = 12 // uint32
	offID         = 16 // uint64
	offLSN        = 24 // uint64
)

var (
	// ErrBadLength is returned when wrapping a byte slice that is not PageSize.
	ErrBadLength = errors.New("page: buffer is not PageSize bytes")
	// ErrBadMagic is returned when a page does not carry the ZadoDB magic.
	ErrBadMagic = errors.New("page: bad magic")
	// ErrBadChecksum is returned when a page's stored checksum does not match.
	ErrBadChecksum = errors.New("page: checksum mismatch (torn or corrupt page)")
)

// Page is a fixed-size page. It wraps a PageSize-length byte slice; for pages
// read from an mmap the slice points directly into the mapping (zero-copy), so
// callers must treat pages obtained via From on a read-only mapping as
// read-only.
type Page struct {
	data []byte
}

// New allocates a fresh, zeroed page with the given id and type.
func New(id PageID, t PageType) *Page {
	p := &Page{data: make([]byte, PageSize)}
	binary.LittleEndian.PutUint32(p.data[offMagic:], magic)
	p.data[offType] = byte(t)
	binary.LittleEndian.PutUint64(p.data[offID:], uint64(id))
	return p
}

// From wraps an existing PageSize-length slice without copying and without
// verifying its checksum (call Verify for that). Use for read paths over mmap.
func From(data []byte) (*Page, error) {
	if len(data) != PageSize {
		return nil, ErrBadLength
	}
	return &Page{data: data}, nil
}

// Bytes returns the underlying PageSize-length slice (checksum must already be
// finalized for write paths).
func (p *Page) Bytes() []byte { return p.data }

// ID returns the page's self-referential id.
func (p *Page) ID() PageID {
	return PageID(binary.LittleEndian.Uint64(p.data[offID:]))
}

// SetID updates the page's id.
func (p *Page) SetID(id PageID) {
	binary.LittleEndian.PutUint64(p.data[offID:], uint64(id))
}

// Type returns the page type.
func (p *Page) Type() PageType { return PageType(p.data[offType]) }

// SetType sets the page type.
func (p *Page) SetType(t PageType) { p.data[offType] = byte(t) }

// Flags returns the page flags byte.
func (p *Page) Flags() uint8 { return p.data[offFlags] }

// SetFlags sets the page flags byte.
func (p *Page) SetFlags(f uint8) { p.data[offFlags] = f }

// PayloadLen returns the number of meaningful bytes in the body.
func (p *Page) PayloadLen() uint32 {
	return binary.LittleEndian.Uint32(p.data[offPayloadLen:])
}

// SetPayloadLen records how many bytes of the body are meaningful.
func (p *Page) SetPayloadLen(n uint32) {
	binary.LittleEndian.PutUint32(p.data[offPayloadLen:], n)
}

// LSN returns the log/sequence number stamped on the page (the checkpoint that
// wrote it). Useful for debugging and ordering.
func (p *Page) LSN() uint64 {
	return binary.LittleEndian.Uint64(p.data[offLSN:])
}

// SetLSN stamps the page with a log/sequence number.
func (p *Page) SetLSN(lsn uint64) {
	binary.LittleEndian.PutUint64(p.data[offLSN:], lsn)
}

// Body returns the page's payload area (length BodySize). The slice aliases the
// page buffer; do not retain it past the page's lifetime.
func (p *Page) Body() []byte { return p.data[HeaderSize:] }

// Payload returns the meaningful prefix of the body (length PayloadLen).
func (p *Page) Payload() []byte {
	n := p.PayloadLen()
	if int(n) > BodySize {
		n = BodySize
	}
	return p.data[HeaderSize : HeaderSize+int(n)]
}

// Finalize computes and writes the checksum over the whole page. Call this
// after all mutations and before persisting the page.
func (p *Page) Finalize() {
	sum := crc32.Checksum(p.data[offMagic:], castagnoli)
	binary.LittleEndian.PutUint32(p.data[offChecksum:], sum)
}

// Verify checks the magic and recomputes the checksum, returning an error if
// the page is not a valid, intact ZadoDB page.
func (p *Page) Verify() error {
	if len(p.data) != PageSize {
		return ErrBadLength
	}
	if binary.LittleEndian.Uint32(p.data[offMagic:]) != magic {
		return ErrBadMagic
	}
	want := binary.LittleEndian.Uint32(p.data[offChecksum:])
	got := crc32.Checksum(p.data[offMagic:], castagnoli)
	if want != got {
		return ErrBadChecksum
	}
	return nil
}
