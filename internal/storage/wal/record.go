// Package wal implements ZadoDB's write-ahead log: the single source of
// durability. Every mutation is appended here as a checksummed record and is
// only acknowledged to the client after a physical fsync. A dedicated
// sequencer goroutine is the one and only writer, which is the engine's single
// point of serialization (everything else — validation, msgpack encoding,
// checksums of the payload — happens concurrently on caller goroutines).
package wal

import (
	"encoding/binary"

	"github.com/vmihailenco/msgpack/v5"
)

// OpType is the kind of mutation carried by a WAL entry.
type OpType uint8

const (
	OpPut         OpType = iota + 1 // create or replace an object
	OpDelete                        // delete an object
	OpCreateClass                   // register a class
	OpDropClass                     // drop a class
)

// WALEntry is the logical mutation stored (msgpack-encoded) in a record's
// payload. It is also the unit replayed during checkpoint and recovery.
type WALEntry struct {
	Op        OpType `msgpack:"op"`
	Class     string `msgpack:"class"`
	ObjectID  int64  `msgpack:"id,omitempty"`
	Data      []byte `msgpack:"data,omitempty"` // object payload (msgpack) for OpPut / class meta for OpCreateClass
	Timestamp int64  `msgpack:"ts"`
}

// Marshal encodes the entry to msgpack. Callers do this off the sequencer's
// critical path.
func (e WALEntry) Marshal() ([]byte, error) { return msgpack.Marshal(e) }

// UnmarshalEntry decodes a msgpack payload back into a WALEntry.
func UnmarshalEntry(payload []byte) (WALEntry, error) {
	var e WALEntry
	err := msgpack.Unmarshal(payload, &e)
	return e, err
}

// Key namespaces keep class definitions and object rows in a single ordered
// keyspace. Class names are validated (no 0x00) at the API layer, so the 0x00
// separator is unambiguous.
const (
	nsClass  = 0x01 // class definition keys sort before all object keys
	nsObject = 0x02 // object row keys
	keySep   = 0x00
)

// ClassKey returns the B+Tree key for a class definition.
func ClassKey(class string) []byte {
	k := make([]byte, 0, 1+len(class))
	k = append(k, nsClass)
	k = append(k, class...)
	return k
}

// ObjectKey returns the B+Tree key for an object. Object ids are auto-increment
// positive int64s, so big-endian encoding sorts them ascending within a class.
func ObjectKey(class string, id int64) []byte {
	k := make([]byte, 0, 1+len(class)+1+8)
	k = append(k, nsObject)
	k = append(k, class...)
	k = append(k, keySep)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(id))
	k = append(k, b[:]...)
	return k
}

// ObjectPrefix returns the key prefix that all of a class's object keys share,
// used for range scans (listing a class).
func ObjectPrefix(class string) []byte {
	k := make([]byte, 0, 1+len(class)+1)
	k = append(k, nsObject)
	k = append(k, class...)
	k = append(k, keySep)
	return k
}

// Key returns the B+Tree key this entry mutates.
func (e WALEntry) Key() []byte {
	switch e.Op {
	case OpCreateClass, OpDropClass:
		return ClassKey(e.Class)
	default:
		return ObjectKey(e.Class, e.ObjectID)
	}
}
