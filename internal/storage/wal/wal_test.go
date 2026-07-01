package wal

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestEntryMarshalRoundTrip(t *testing.T) {
	e := WALEntry{Op: OpPut, Class: "Pessoa", ObjectID: 7, Data: []byte("payload"), Timestamp: 123}
	b, err := e.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := UnmarshalEntry(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Op != e.Op || got.Class != e.Class || got.ObjectID != e.ObjectID ||
		!bytes.Equal(got.Data, e.Data) || got.Timestamp != e.Timestamp {
		t.Errorf("round trip = %+v, want %+v", got, e)
	}
}

func TestObjectKeyOrdering(t *testing.T) {
	// Object keys of the same class must sort ascending by id.
	k1 := ObjectKey("A", 1)
	k2 := ObjectKey("A", 2)
	k10 := ObjectKey("A", 10)
	if !(bytes.Compare(k1, k2) < 0 && bytes.Compare(k2, k10) < 0) {
		t.Errorf("object keys not ascending by id")
	}
	// Class definition keys sort before object keys of the same class.
	if bytes.Compare(ClassKey("A"), ObjectKey("A", 1)) >= 0 {
		t.Errorf("class key should sort before object keys")
	}
	// Object keys carry the class prefix.
	if !bytes.HasPrefix(ObjectKey("A", 5), ObjectPrefix("A")) {
		t.Errorf("object key missing class prefix")
	}
}

func writeAndSync(t *testing.T, w *Writer, txID uint64, payload []byte) {
	t.Helper()
	if err := w.Append(txID, payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func TestWriterReaderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	payloads := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	for i, p := range payloads {
		writeAndSync(t, w, uint64(i+1), p)
	}
	w.Close()

	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()
	for i, want := range payloads {
		txID, got, err := r.Next()
		if err != nil {
			t.Fatalf("Next[%d]: %v", i, err)
		}
		if txID != uint64(i+1) {
			t.Errorf("txID[%d] = %d, want %d", i, txID, i+1)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("payload[%d] = %q, want %q", i, got, want)
		}
	}
	if _, _, err := r.Next(); err != io.EOF {
		t.Errorf("final Next = %v, want io.EOF", err)
	}
}

func TestReaderStopsAtTruncatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	writeAndSync(t, w, 1, []byte("good record"))
	goodLen := w.Offset()
	writeAndSync(t, w, 2, []byte("this one will be truncated"))
	w.Close()

	// Simulate a crash mid-write of the second record: chop the file so only
	// the first record and a partial second remain.
	if err := os.Truncate(path, goodLen+10); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()

	txID, payload, err := r.Next()
	if err != nil {
		t.Fatalf("Next[0]: %v", err)
	}
	if txID != 1 || string(payload) != "good record" {
		t.Errorf("first record = (%d,%q), want (1,%q)", txID, payload, "good record")
	}
	if _, _, err := r.Next(); err != ErrCorrupt {
		t.Errorf("torn tail Next = %v, want ErrCorrupt", err)
	}
	// The durable prefix ends exactly after the first record.
	if r.Offset() != goodLen {
		t.Errorf("durable offset = %d, want %d", r.Offset(), goodLen)
	}
}

func TestReaderDetectsCorruptedPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, _ := OpenWriter(path)
	writeAndSync(t, w, 1, []byte("hello there general kenobi"))
	w.Close()

	// Flip a byte in the payload region (past the 24-byte header).
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	buf := make([]byte, 1)
	f.ReadAt(buf, recHeaderLen+3)
	buf[0] ^= 0xFF
	f.WriteAt(buf, recHeaderLen+3)
	f.Close()

	r, _ := OpenReader(path)
	defer r.Close()
	if _, _, err := r.Next(); err != ErrCorrupt {
		t.Errorf("corrupted payload Next = %v, want ErrCorrupt", err)
	}
}

func testSequencerConcurrency(t *testing.T, mode FsyncMode) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	seq := NewSequencer(w, mode, 0, 4096)

	const writers = 50
	const perWriter = 40
	var mu sync.Mutex
	seen := make([]uint64, 0, writers*perWriter)

	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				payload := []byte("w")
				txID, err := seq.Submit(payload)
				if err != nil {
					t.Errorf("Submit: %v", err)
					return
				}
				mu.Lock()
				seen = append(seen, txID)
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	if err := seq.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Every TxID must be unique and the set must be exactly 1..N.
	if len(seen) != writers*perWriter {
		t.Fatalf("got %d txIDs, want %d", len(seen), writers*perWriter)
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i] < seen[j] })
	for i, tx := range seen {
		if tx != uint64(i+1) {
			t.Fatalf("txID[%d] = %d, want %d (duplicate or gap)", i, tx, i+1)
		}
	}

	// All records must be readable back from disk.
	r, _ := OpenReader(path)
	defer r.Close()
	count := 0
	for {
		_, _, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("replay Next: %v", err)
		}
		count++
	}
	if count != writers*perWriter {
		t.Errorf("replayed %d records, want %d", count, writers*perWriter)
	}
}

func TestSequencerConcurrentPerCommit(t *testing.T) {
	testSequencerConcurrency(t, DefaultFsyncMode())
}

func TestSequencerConcurrentGroupCommit(t *testing.T) {
	testSequencerConcurrency(t, GroupCommitMode(2*time.Millisecond, 128))
}

func TestSubmitAfterCloseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, _ := OpenWriter(path)
	seq := NewSequencer(w, DefaultFsyncMode(), 0, 16)
	if _, err := seq.Submit([]byte("x")); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	seq.Close()
	if _, err := seq.Submit([]byte("y")); err != ErrClosed {
		t.Errorf("Submit after Close = %v, want ErrClosed", err)
	}
}
