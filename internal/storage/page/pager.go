package page

import (
	"encoding/binary"
	"fmt"
	"os"
)

// Meta is the persistent root record stored in the meta page (PageID 0). It
// points at the current B+Tree root and records how far the WAL has been
// applied into this data file.
//
// The engine deliberately does NOT persist per-class id counters here: they are
// reconstructed at boot by scanning the tree and replaying the WAL (see idgen),
// which keeps the meta page fixed-size and avoids a second source of truth.
type Meta struct {
	Root            PageID // root of the B+Tree (InvalidPageID when empty)
	LastAppliedTxID uint64 // highest WAL TxID durably folded into this file
	NumPages        uint64 // total pages allocated in the file
}

const metaMagic = 0x5A4D4554 // "ZMET"

// encode writes the meta record into a page body.
func (m Meta) encode(p *Page) {
	b := p.Body()
	binary.LittleEndian.PutUint32(b[0:], metaMagic)
	binary.LittleEndian.PutUint64(b[4:], uint64(m.Root))
	binary.LittleEndian.PutUint64(b[12:], m.LastAppliedTxID)
	binary.LittleEndian.PutUint64(b[20:], m.NumPages)
	p.SetPayloadLen(28)
}

// decodeMeta reads a meta record from a page body.
func decodeMeta(p *Page) (Meta, error) {
	b := p.Body()
	if binary.LittleEndian.Uint32(b[0:]) != metaMagic {
		return Meta{}, fmt.Errorf("page: bad meta magic")
	}
	return Meta{
		Root:            PageID(binary.LittleEndian.Uint64(b[4:])),
		LastAppliedTxID: binary.LittleEndian.Uint64(b[12:]),
		NumPages:        binary.LittleEndian.Uint64(b[20:]),
	}, nil
}

// Manager owns a data file and hands out sequential pages. It is used only by
// the checkpoint and recovery paths that build/mutate the data file; live reads
// go through the mmap-backed snapshot (package mvcc), never through Manager.
//
// Allocation is purely sequential (no free list): copy-on-write orphans are
// simply wasted space until the next checkpoint, which rewrites a fresh file
// and reclaims everything. This keeps the pager simple and correct.
type Manager struct {
	f        *os.File
	numPages uint64
}

// Create makes a brand-new data file with an empty meta page (root =
// InvalidPageID). It fails if the file already exists.
func Create(path string) (*Manager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("page: create %s: %w", path, err)
	}
	m := &Manager{f: f, numPages: 1} // page 0 reserved for meta
	meta := Meta{Root: InvalidPageID, LastAppliedTxID: 0, NumPages: 1}
	if err := m.WriteMeta(meta); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := m.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return m, nil
}

// Open opens an existing data file and reads its meta page to learn the page
// count.
func Open(path string) (*Manager, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("page: open %s: %w", path, err)
	}
	m := &Manager{f: f}
	meta, err := m.ReadMeta()
	if err != nil {
		f.Close()
		return nil, err
	}
	m.numPages = meta.NumPages
	if m.numPages == 0 {
		m.numPages = 1
	}
	return m, nil
}

// Close closes the underlying file.
func (m *Manager) Close() error { return m.f.Close() }

// NumPages returns the number of pages currently allocated in the file.
func (m *Manager) NumPages() uint64 { return m.numPages }

// Allocate reserves the next sequential page id. The page is not written until
// WritePage is called.
func (m *Manager) Allocate() PageID {
	id := PageID(m.numPages)
	m.numPages++
	return id
}

// WritePage writes a finalized page to its id's offset. The page's checksum
// must already be set (call Page.Finalize first); WritePage finalizes as a
// safety net.
func (m *Manager) WritePage(p *Page) error {
	p.Finalize()
	off := int64(p.ID()) * PageSize
	if _, err := m.f.WriteAt(p.Bytes(), off); err != nil {
		return fmt.Errorf("page: write page %d: %w", p.ID(), err)
	}
	return nil
}

// ReadPage reads and verifies the page at the given id.
func (m *Manager) ReadPage(id PageID) (*Page, error) {
	buf := make([]byte, PageSize)
	off := int64(id) * PageSize
	if _, err := m.f.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("page: read page %d: %w", id, err)
	}
	p, err := From(buf)
	if err != nil {
		return nil, err
	}
	if err := p.Verify(); err != nil {
		return nil, fmt.Errorf("page: read page %d: %w", id, err)
	}
	return p, nil
}

// WriteMeta writes the meta record to page 0 (with the current page count).
func (m *Manager) WriteMeta(meta Meta) error {
	if meta.NumPages == 0 {
		meta.NumPages = m.numPages
	}
	m.numPages = meta.NumPages
	p := New(MetaPageID, PageTypeMeta)
	meta.encode(p)
	return m.WritePage(p)
}

// ReadMeta reads the meta record from page 0.
func (m *Manager) ReadMeta() (Meta, error) {
	p, err := m.ReadPage(MetaPageID)
	if err != nil {
		return Meta{}, err
	}
	if p.Type() != PageTypeMeta {
		return Meta{}, fmt.Errorf("page: page 0 is not a meta page (type %s)", p.Type())
	}
	return decodeMeta(p)
}

// Sync flushes the file to stable storage (fsync / FlushFileBuffers).
func (m *Manager) Sync() error {
	if err := m.f.Sync(); err != nil {
		return fmt.Errorf("page: sync: %w", err)
	}
	return nil
}
