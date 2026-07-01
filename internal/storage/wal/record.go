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
	OpBatch                         // atomic batch: apply Sub entries all-or-nothing
)

// WALEntry is the logical mutation stored (msgpack-encoded) in a record's
// payload. It is also the unit replayed during checkpoint and recovery.
//
// An OpBatch entry carries N sub-mutations in Sub. Because the whole batch is a
// single WAL record (one CRC, one fsync), it is inherently atomic: recovery
// either reads the complete record and applies all sub-entries, or the record
// is torn and the entire batch is dropped — never a partial apply.
type WALEntry struct {
	Op        OpType     `msgpack:"op"`
	Project   string     `msgpack:"prj,omitempty"` // virtual namespace; "" is the default project
	Class     string     `msgpack:"class"`
	ObjectID  int64      `msgpack:"id,omitempty"`
	Data      []byte     `msgpack:"data,omitempty"` // object payload (msgpack) for OpPut / class meta for OpCreateClass
	Timestamp int64      `msgpack:"ts"`
	Sub       []WALEntry `msgpack:"sub,omitempty"` // sub-mutations for OpBatch
}

// Flatten returns the effective sub-mutations of an entry: an OpBatch expands to
// its Sub entries; any other entry is itself. Used so apply/replay/overlay all
// treat a batch as its constituent operations.
func (e WALEntry) Flatten() []WALEntry {
	if e.Op == OpBatch {
		return e.Sub
	}
	return []WALEntry{e}
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
// keyspace. Class and project names are validated (no 0x00) at the API layer,
// so the 0x00 separators are unambiguous.
const (
	nsClass  = 0x01 // class definition keys sort before all object keys
	nsObject = 0x02 // object row keys
	keySep   = 0x00
)

// A project is a virtual namespace grouping a set of classes. The default
// project is the empty string "" and uses the LEGACY key layout with no project
// prefix, so a database written before projects existed needs no migration. A
// named project prepends "project + 0x00" to the class body. Because names never
// contain 0x00, a key belongs to the default project iff its body carries no
// 0x00 separator before the (object) id, and to a named project otherwise.

// scope returns the "project + 0x00" prefix for a named project, or nil for the
// default project (so its keys keep the legacy layout).
func scope(project string) []byte {
	if project == "" {
		return nil
	}
	b := make([]byte, 0, len(project)+1)
	b = append(b, project...)
	b = append(b, keySep)
	return b
}

// ClassKey returns the B+Tree key for a class definition within a project.
func ClassKey(project, class string) []byte {
	sp := scope(project)
	k := make([]byte, 0, 1+len(sp)+len(class))
	k = append(k, nsClass)
	k = append(k, sp...)
	k = append(k, class...)
	return k
}

// ObjectKey returns the B+Tree key for an object. Object ids are auto-increment
// positive int64s, so big-endian encoding sorts them ascending within a class.
func ObjectKey(project, class string, id int64) []byte {
	sp := scope(project)
	k := make([]byte, 0, 1+len(sp)+len(class)+1+8)
	k = append(k, nsObject)
	k = append(k, sp...)
	k = append(k, class...)
	k = append(k, keySep)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(id))
	k = append(k, b[:]...)
	return k
}

// ObjectPrefix returns the key prefix that all of a class's object keys share,
// used for range scans (listing a class).
func ObjectPrefix(project, class string) []byte {
	sp := scope(project)
	k := make([]byte, 0, 1+len(sp)+len(class)+1)
	k = append(k, nsObject)
	k = append(k, sp...)
	k = append(k, class...)
	k = append(k, keySep)
	return k
}

// splitScope splits a key body (project/class portion, no namespace byte and no
// trailing id) into its project and class. A body with no 0x00 belongs to the
// default project; one with a 0x00 carries "project + 0x00 + class".
func splitScope(body []byte) (project, class string) {
	if i := indexZero(body); i >= 0 {
		return string(body[:i]), string(body[i+1:])
	}
	return "", string(body)
}

func indexZero(b []byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == keySep {
			return i
		}
	}
	return -1
}

// DecodeObjectKey parses an object key back into its project, class and id. ok
// is false for keys that are not object keys (e.g. class-definition keys). Used
// at boot to reseed the id generator from the data already on disk.
func DecodeObjectKey(key []byte) (project, class string, id int64, ok bool) {
	// [nsObject] + [project + keySep] + class + [keySep] + id(8)
	if len(key) < 1+1+8 || key[0] != nsObject {
		return "", "", 0, false
	}
	sepPos := len(key) - 9
	if key[sepPos] != keySep {
		return "", "", 0, false
	}
	project, class = splitScope(key[1:sepPos])
	id = int64(binary.BigEndian.Uint64(key[sepPos+1:]))
	return project, class, id, true
}

// DecodeClassKey parses a class-definition key back into its project and class
// name. ok is false for keys that are not class keys.
func DecodeClassKey(key []byte) (project, class string, ok bool) {
	if len(key) < 1 || key[0] != nsClass {
		return "", "", false
	}
	project, class = splitScope(key[1:])
	return project, class, true
}

// ScopeKey is the composite identity "project + 0x00 + class" used as an
// in-memory map key (class set, id generator) to keep the same class name in
// different projects independent. Names never contain 0x00, so it is unambiguous.
func ScopeKey(project, class string) string { return project + string(rune(keySep)) + class }

// Key returns the B+Tree key this entry mutates.
func (e WALEntry) Key() []byte {
	switch e.Op {
	case OpCreateClass, OpDropClass:
		return ClassKey(e.Project, e.Class)
	default:
		return ObjectKey(e.Project, e.Class, e.ObjectID)
	}
}
