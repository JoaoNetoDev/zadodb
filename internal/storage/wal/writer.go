package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
)

// On-disk record framing:
//
//	[0:4]   recMagic uint32       identifies a record boundary
//	[4:5]   version  uint8
//	[5:8]   reserved (3 bytes, zero)
//	[8:16]  TxID     uint64
//	[16:20] PayloadLen uint32
//	[20:24] CRC32C   uint32       over TxID(8) || PayloadLen(4) || Payload
//	[24:..] Payload  []byte       msgpack-encoded WALEntry
//
// A record is only meaningful once its bytes are fully written and fsynced. A
// crash mid-write leaves a torn tail that the Reader detects (bad magic, short
// read, or CRC mismatch) and treats as the end of the log.
const (
	recMagic     = 0x5A57414C // "ZWAL"
	recVersion   = 1
	recHeaderLen = 24
	// MaxRecordLen bounds a single payload so a corrupted length field cannot
	// trigger a huge allocation during recovery.
	MaxRecordLen = 64 << 20 // 64 MiB
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Writer appends framed records to the WAL file. It is not safe for concurrent
// use; only the Sequencer writes to it.
type Writer struct {
	f      *os.File
	offset int64
}

// OpenWriter opens (creating if needed) the WAL file for appending.
func OpenWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("wal: open writer %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("wal: stat %s: %w", path, err)
	}
	return &Writer{f: f, offset: info.Size()}, nil
}

// Append writes a single framed record. It does not fsync; the Sequencer
// batches fsyncs according to the configured mode.
func (w *Writer) Append(txID uint64, payload []byte) error {
	if len(payload) > MaxRecordLen {
		return fmt.Errorf("wal: payload too large: %d > %d", len(payload), MaxRecordLen)
	}
	var hdr [recHeaderLen]byte
	binary.LittleEndian.PutUint32(hdr[0:], recMagic)
	hdr[4] = recVersion
	binary.LittleEndian.PutUint64(hdr[8:], txID)
	binary.LittleEndian.PutUint32(hdr[16:], uint32(len(payload)))

	// CRC over TxID || PayloadLen || Payload (bytes 8:20 of the header + body).
	crc := crc32.Checksum(hdr[8:20], crcTable)
	crc = crc32.Update(crc, crcTable, payload)
	binary.LittleEndian.PutUint32(hdr[20:], crc)

	// O_APPEND makes each Write land atomically at end-of-file.
	if _, err := w.f.Write(hdr[:]); err != nil {
		return fmt.Errorf("wal: append header: %w", err)
	}
	if _, err := w.f.Write(payload); err != nil {
		return fmt.Errorf("wal: append payload: %w", err)
	}
	w.offset += int64(recHeaderLen + len(payload))
	return nil
}

// Sync flushes buffered writes to stable storage (fsync / FlushFileBuffers).
func (w *Writer) Sync() error {
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("wal: sync: %w", err)
	}
	return nil
}

// Offset returns the current end-of-log byte offset.
func (w *Writer) Offset() int64 { return w.offset }

// Close closes the WAL file.
func (w *Writer) Close() error { return w.f.Close() }
