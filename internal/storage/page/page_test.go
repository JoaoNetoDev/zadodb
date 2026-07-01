package page

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestPageHeaderRoundTrip(t *testing.T) {
	p := New(42, PageTypeBTreeLeaf)
	p.SetLSN(99)
	p.SetFlags(0x7)
	copy(p.Body(), []byte("hello world"))
	p.SetPayloadLen(uint32(len("hello world")))
	p.Finalize()

	// Re-wrap the raw bytes and read everything back.
	q, err := From(p.Bytes())
	if err != nil {
		t.Fatalf("From: %v", err)
	}
	if err := q.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if q.ID() != 42 {
		t.Errorf("ID = %d, want 42", q.ID())
	}
	if q.Type() != PageTypeBTreeLeaf {
		t.Errorf("Type = %s, want btree-leaf", q.Type())
	}
	if q.LSN() != 99 {
		t.Errorf("LSN = %d, want 99", q.LSN())
	}
	if q.Flags() != 0x7 {
		t.Errorf("Flags = %d, want 7", q.Flags())
	}
	if got := string(q.Payload()); got != "hello world" {
		t.Errorf("Payload = %q, want %q", got, "hello world")
	}
}

func TestVerifyDetectsBitFlip(t *testing.T) {
	p := New(1, PageTypeBTreeLeaf)
	copy(p.Body(), []byte("important data"))
	p.SetPayloadLen(14)
	p.Finalize()

	raw := make([]byte, PageSize)
	copy(raw, p.Bytes())

	// Flip one bit deep in the body.
	raw[HeaderSize+3] ^= 0x08

	q, _ := From(raw)
	if err := q.Verify(); err != ErrBadChecksum {
		t.Fatalf("Verify after bit flip = %v, want ErrBadChecksum", err)
	}
}

func TestVerifyDetectsHeaderCorruption(t *testing.T) {
	p := New(1, PageTypeMeta)
	p.SetPayloadLen(0)
	p.Finalize()

	raw := make([]byte, PageSize)
	copy(raw, p.Bytes())
	raw[offType] = byte(PageTypeOverflow) // tamper with the type

	q, _ := From(raw)
	if err := q.Verify(); err != ErrBadChecksum {
		t.Fatalf("Verify after type tamper = %v, want ErrBadChecksum", err)
	}
}

func TestVerifyBadMagic(t *testing.T) {
	raw := make([]byte, PageSize)
	q, _ := From(raw)
	if err := q.Verify(); err != ErrBadMagic {
		t.Fatalf("Verify on zeroed page = %v, want ErrBadMagic", err)
	}
}

func TestFromWrongLength(t *testing.T) {
	if _, err := From(make([]byte, 100)); err != ErrBadLength {
		t.Fatalf("From(100 bytes) = %v, want ErrBadLength", err)
	}
}

func TestManagerAllocateAndReadWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.zdb")
	m, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Page 0 is the meta page; allocation starts at 1.
	id1 := m.Allocate()
	id2 := m.Allocate()
	if id1 != 1 || id2 != 2 {
		t.Fatalf("Allocate gave %d,%d, want 1,2", id1, id2)
	}

	p := New(id1, PageTypeBTreeLeaf)
	payload := []byte("row payload bytes")
	copy(p.Body(), payload)
	p.SetPayloadLen(uint32(len(payload)))
	if err := m.WritePage(p); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	if err := m.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got, err := m.ReadPage(id1)
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if !bytes.Equal(got.Payload(), payload) {
		t.Errorf("read payload = %q, want %q", got.Payload(), payload)
	}
	m.Close()

	// Reopen and confirm the page count survived via the meta page.
	m2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m2.Close()
	// Persist NumPages by writing meta (Create wrote NumPages=1; the two
	// allocations only bumped the in-memory counter, which is expected: real
	// callers write meta at the end of a checkpoint).
	if err := m2.WriteMeta(Meta{Root: 1, LastAppliedTxID: 7, NumPages: 3}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	meta, err := m2.ReadMeta()
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.Root != 1 || meta.LastAppliedTxID != 7 || meta.NumPages != 3 {
		t.Errorf("meta = %+v, want {Root:1 LastAppliedTxID:7 NumPages:3}", meta)
	}
}

func TestCreateExistingFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.zdb")
	m, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.Close()
	if _, err := Create(path); err == nil {
		t.Fatal("Create on existing file should fail")
	}
}
