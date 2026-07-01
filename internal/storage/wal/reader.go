package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// ErrCorrupt signals a torn or corrupt record at the tail of the log. During
// recovery this is the EXPECTED outcome of a crash mid-write: the records
// before it are durable and valid; this one (and anything after) is discarded.
// It is not a fatal error.
var ErrCorrupt = errors.New("wal: torn or corrupt record (end of durable log)")

// Reader reads framed records sequentially from a WAL file.
type Reader struct {
	f      *os.File
	r      *bufio.Reader
	offset int64
}

// OpenReader opens a WAL file for sequential reading from the beginning.
func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("wal: open reader %s: %w", path, err)
	}
	return &Reader{f: f, r: bufio.NewReaderSize(f, 1<<16)}, nil
}

// Offset returns the byte offset just past the last successfully returned
// record — i.e. the length of the durable prefix of the log.
func (r *Reader) Offset() int64 { return r.offset }

// Next returns the next record. It returns io.EOF at a clean end of log, and
// ErrCorrupt when it hits a torn/corrupt tail (both mean "stop replaying").
func (r *Reader) Next() (txID uint64, payload []byte, err error) {
	var hdr [recHeaderLen]byte
	n, err := io.ReadFull(r.r, hdr[:])
	if err == io.EOF && n == 0 {
		return 0, nil, io.EOF // clean end
	}
	if err != nil {
		// Partial header at EOF, or read error: torn tail.
		return 0, nil, ErrCorrupt
	}
	if binary.LittleEndian.Uint32(hdr[0:]) != recMagic {
		return 0, nil, ErrCorrupt
	}
	if hdr[4] != recVersion {
		return 0, nil, fmt.Errorf("wal: unsupported record version %d", hdr[4])
	}
	txID = binary.LittleEndian.Uint64(hdr[8:])
	plen := binary.LittleEndian.Uint32(hdr[16:])
	if plen > MaxRecordLen {
		return 0, nil, ErrCorrupt
	}
	wantCRC := binary.LittleEndian.Uint32(hdr[20:])

	payload = make([]byte, plen)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return 0, nil, ErrCorrupt // truncated payload
	}
	crc := crc32.Checksum(hdr[8:20], crcTable)
	crc = crc32.Update(crc, crcTable, payload)
	if crc != wantCRC {
		return 0, nil, ErrCorrupt
	}
	r.offset += int64(recHeaderLen) + int64(plen)
	return txID, payload, nil
}

// Close closes the WAL file.
func (r *Reader) Close() error { return r.f.Close() }
