// Package mvcc provides the read path: an immutable, memory-mapped snapshot of
// a data-file generation. Reads navigate the B+Tree directly over the mmap
// (page-cache dereferences, no disk parsing, no locks) and never touch the WAL
// or the write path.
//
// A MappedFile holds the current Snapshot behind an atomic pointer. After a
// checkpoint publishes a new generation, SwapTo installs a fresh snapshot; the
// old one is unmapped only once its last in-flight reader releases it.
package mvcc

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/JoaoNetoDev/zadodb/internal/storage/btree"
	"github.com/JoaoNetoDev/zadodb/internal/storage/page"
	mmap "github.com/edsrzf/mmap-go"
)

// Snapshot is an immutable view of one data-file generation.
type Snapshot struct {
	f    *os.File
	mm   mmap.MMap
	data []byte
	root page.PageID

	refs      atomic.Int64
	retired   atomic.Bool
	unmapOnce sync.Once
}

// openSnapshot maps a data file read-only and reads its root from the meta page.
func openSnapshot(path string) (*Snapshot, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("mvcc: open %s: %w", path, err)
	}
	mm, err := mmap.Map(f, mmap.RDONLY, 0)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mvcc: mmap %s: %w", path, err)
	}
	s := &Snapshot{f: f, mm: mm, data: mm}
	metaPage, err := s.ReadPage(page.MetaPageID)
	if err != nil {
		mm.Unmap()
		f.Close()
		return nil, err
	}
	meta, err := page.DecodeMeta(metaPage)
	if err != nil {
		mm.Unmap()
		f.Close()
		return nil, err
	}
	s.root = meta.Root
	return s, nil
}

// ReadPage returns the page at id as a zero-copy view into the mapping. The
// returned page (and any slice from it) must not be retained past Release.
func (s *Snapshot) ReadPage(id page.PageID) (*page.Page, error) {
	off := int64(id) * page.PageSize
	if off < 0 || off+page.PageSize > int64(len(s.data)) {
		return nil, fmt.Errorf("mvcc: page %d out of range", id)
	}
	p, err := page.From(s.data[off : off+page.PageSize])
	if err != nil {
		return nil, err
	}
	if err := p.Verify(); err != nil {
		return nil, fmt.Errorf("mvcc: page %d: %w", id, err)
	}
	return p, nil
}

// Root returns the B+Tree root of this snapshot.
func (s *Snapshot) Root() page.PageID { return s.root }

// Get looks up key. Returned values are copied out of the mapping.
func (s *Snapshot) Get(key []byte) ([]byte, bool, error) {
	return btree.Get(s, s.root, key)
}

// Scan visits every key/value with the given prefix in ascending order.
func (s *Snapshot) Scan(prefix []byte, fn func(key, value []byte) bool) error {
	return btree.Scan(s, s.root, prefix, fn)
}

// Release drops this reader's reference. When a retired snapshot's last
// reference is released, its mapping is unmapped.
func (s *Snapshot) Release() {
	if s.refs.Add(-1) == 0 && s.retired.Load() {
		s.unmap()
	}
}

func (s *Snapshot) unmap() {
	s.unmapOnce.Do(func() {
		s.mm.Unmap()
		s.f.Close()
	})
}

// MappedFile owns the currently active snapshot and swaps it atomically.
type MappedFile struct {
	current atomic.Pointer[Snapshot]
}

// Open maps the given data file as the initial active snapshot.
func Open(path string) (*MappedFile, error) {
	s, err := openSnapshot(path)
	if err != nil {
		return nil, err
	}
	mf := &MappedFile{}
	mf.current.Store(s)
	return mf, nil
}

// Acquire returns the current snapshot with a reference held; the caller must
// call Release on it when done. It always returns the newest snapshot even
// under a concurrent SwapTo.
func (mf *MappedFile) Acquire() *Snapshot {
	for {
		s := mf.current.Load()
		s.refs.Add(1)
		if mf.current.Load() == s {
			return s
		}
		// A SwapTo slipped in between; drop and retry for the newest.
		s.Release()
	}
}

// SwapTo installs a fresh snapshot over the new path and retires the old one.
// The old mapping is unmapped immediately if idle, otherwise by its last reader.
func (mf *MappedFile) SwapTo(path string) error {
	ns, err := openSnapshot(path)
	if err != nil {
		return err
	}
	old := mf.current.Swap(ns)
	if old != nil {
		old.retired.Store(true)
		if old.refs.Load() == 0 {
			old.unmap()
		}
	}
	return nil
}

// Close unmaps the active snapshot.
func (mf *MappedFile) Close() error {
	if s := mf.current.Swap(nil); s != nil {
		s.retired.Store(true)
		s.unmap()
	}
	return nil
}
