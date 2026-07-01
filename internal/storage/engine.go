// Package storage ties the pieces together into a usable object store: the WAL
// sequencer (durability), the mmap snapshot (reads), the id generator, an
// in-memory overlay of not-yet-checkpointed writes, and a background
// checkpointer.
//
// Write path: validate -> msgpack is done by the caller -> Submit to the
// sequencer (blocks only until that commit's fsync) -> update the overlay.
// Read path: consult the overlay first (newest writes), then the mmap snapshot.
package storage

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/storage/checkpoint"
	"github.com/JoaoNetoDev/zadodb/internal/storage/idgen"
	"github.com/JoaoNetoDev/zadodb/internal/storage/layout"
	"github.com/JoaoNetoDev/zadodb/internal/storage/mvcc"
	"github.com/JoaoNetoDev/zadodb/internal/storage/recovery"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

// Sentinel errors surfaced to the API layer.
var (
	ErrClassExists   = errors.New("class already exists")
	ErrNoClass       = errors.New("class does not exist")
	ErrClassNotEmpty = errors.New("class is not empty")
	ErrNotFound      = errors.New("object not found")
	ErrInvalidName   = errors.New("invalid class name")
)

// Config configures the storage engine.
type Config struct {
	Dir                string
	Fsync              wal.FsyncMode
	CheckpointWALBytes int64         // checkpoint when the WAL grows past this (0 = default 64MiB)
	CheckpointInterval time.Duration // periodic checkpoint if writes are pending (0 = default 5m)
	QueueDepth         int           // sequencer submission queue depth
}

func (c *Config) withDefaults() {
	if c.CheckpointWALBytes <= 0 {
		c.CheckpointWALBytes = 64 << 20
	}
	if c.CheckpointInterval <= 0 {
		c.CheckpointInterval = 5 * time.Minute
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = 4096
	}
}

// Object is one stored object: its id and opaque value bytes.
type Object struct {
	ID   int64
	Data []byte
}

// Stats is a snapshot of engine counters.
type Stats struct {
	LastTxID     uint64
	WALBytes     int64
	ActiveGen    uint64
	NumClasses   int
	OverlaySize  int
	Checkpoints  uint64
	LastCheckpnt time.Time
}

type overlayVal struct {
	txID    uint64
	data    []byte
	deleted bool
}

// Engine is the object store.
type Engine struct {
	cfg    Config
	seq    *wal.Sequencer
	mapped *mvcc.MappedFile
	idgen  *idgen.Generator

	mu        sync.RWMutex
	overlay   map[string]overlayVal
	classes   map[string]struct{}
	activeGen uint64

	ckMu         sync.Mutex // serializes checkpoints
	checkpoints  uint64
	lastCheckpnt time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Open recovers the database in cfg.Dir and starts the engine.
func Open(cfg Config) (*Engine, error) {
	cfg.withDefaults()

	res, err := recovery.Recover(cfg.Dir)
	if err != nil {
		return nil, err
	}
	mapped, err := mvcc.Open(res.ActiveDataPath)
	if err != nil {
		return nil, err
	}
	w, err := wal.OpenWriter(layout.WALFile(cfg.Dir))
	if err != nil {
		mapped.Close()
		return nil, err
	}
	seq := wal.NewSequencer(w, cfg.Fsync, res.LastTxID, cfg.QueueDepth)

	e := &Engine{
		cfg:       cfg,
		seq:       seq,
		mapped:    mapped,
		idgen:     res.Gen,
		overlay:   make(map[string]overlayVal),
		classes:   make(map[string]struct{}),
		activeGen: res.ActiveGen,
		stopCh:    make(chan struct{}),
	}
	if err := e.loadClasses(); err != nil {
		e.Close()
		return nil, err
	}
	e.applyReplay(res.Replayed)

	e.wg.Add(1)
	go e.checkpointLoop()
	return e, nil
}

// loadClasses seeds the in-memory class set from class-definition keys already
// stored in the active generation.
func (e *Engine) loadClasses() error {
	snap := e.mapped.Acquire()
	defer snap.Release()
	return snap.Scan([]byte{0x01}, func(key, _ []byte) bool {
		if name, ok := wal.DecodeClassKey(key); ok {
			e.classes[name] = struct{}{}
		}
		return true
	})
}

// applyReplay folds the recovered overlay (not-yet-checkpointed writes) into
// the live overlay and class set.
func (e *Engine) applyReplay(replayed []recovery.ReplayedEntry) {
	for _, r := range replayed {
		key := string(r.Entry.Key())
		switch r.Entry.Op {
		case wal.OpPut:
			e.overlay[key] = overlayVal{txID: r.TxID, data: r.Entry.Data}
		case wal.OpDelete:
			e.overlay[key] = overlayVal{txID: r.TxID, deleted: true}
		case wal.OpCreateClass:
			e.overlay[key] = overlayVal{txID: r.TxID, data: r.Entry.Data}
			e.classes[r.Entry.Class] = struct{}{}
		case wal.OpDropClass:
			e.overlay[key] = overlayVal{txID: r.TxID, deleted: true}
			delete(e.classes, r.Entry.Class)
		}
	}
}

// Close stops the background checkpointer and releases resources.
func (e *Engine) Close() error {
	select {
	case <-e.stopCh:
	default:
		close(e.stopCh)
	}
	e.wg.Wait()
	if e.seq != nil {
		e.seq.Close()
	}
	if e.mapped != nil {
		e.mapped.Close()
	}
	return nil
}

// submit marshals and appends an entry, then records it in the overlay under
// the same critical section so reads never see a gap.
func (e *Engine) submit(entry wal.WALEntry, deleted bool) (uint64, error) {
	entry.Timestamp = time.Now().UnixNano()
	payload, err := entry.Marshal()
	if err != nil {
		return 0, err
	}
	txID, err := e.seq.Submit(payload)
	if err != nil {
		return 0, err
	}
	key := string(entry.Key())
	e.mu.Lock()
	e.overlay[key] = overlayVal{txID: txID, data: entry.Data, deleted: deleted}
	e.mu.Unlock()
	return txID, nil
}

// lookup returns the current value for a key, consulting the overlay first.
func (e *Engine) lookup(key []byte) ([]byte, bool, error) {
	e.mu.RLock()
	ov, ok := e.overlay[string(key)]
	e.mu.RUnlock()
	if ok {
		if ov.deleted {
			return nil, false, nil
		}
		return append([]byte(nil), ov.data...), true, nil
	}
	snap := e.mapped.Acquire()
	defer snap.Release()
	return snap.Get(key)
}

// ---- Classes ----

// CreateClass registers a new class.
func (e *Engine) CreateClass(name string) error {
	if !validClassName(name) {
		return ErrInvalidName
	}
	e.mu.RLock()
	_, exists := e.classes[name]
	e.mu.RUnlock()
	if exists {
		return ErrClassExists
	}
	if _, err := e.submit(wal.WALEntry{Op: wal.OpCreateClass, Class: name}, false); err != nil {
		return err
	}
	e.mu.Lock()
	e.classes[name] = struct{}{}
	e.mu.Unlock()
	return nil
}

// ListClasses returns the existing class names, sorted.
func (e *Engine) ListClasses() []string {
	e.mu.RLock()
	out := make([]string, 0, len(e.classes))
	for c := range e.classes {
		out = append(out, c)
	}
	e.mu.RUnlock()
	sort.Strings(out)
	return out
}

// ClassExists reports whether a class exists.
func (e *Engine) ClassExists(name string) bool {
	e.mu.RLock()
	_, ok := e.classes[name]
	e.mu.RUnlock()
	return ok
}

// DropClass removes an empty class.
func (e *Engine) DropClass(name string) error {
	if !e.ClassExists(name) {
		return ErrNoClass
	}
	// Reject if any object of the class remains.
	objs, err := e.ListObjects(name, 1, 0)
	if err != nil {
		return err
	}
	if len(objs) > 0 {
		return ErrClassNotEmpty
	}
	if _, err := e.submit(wal.WALEntry{Op: wal.OpDropClass, Class: name}, true); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.classes, name)
	e.mu.Unlock()
	e.idgen.Drop(name)
	return nil
}

// ---- Objects ----

// CreateObject assigns a new id and stores data, returning the id.
func (e *Engine) CreateObject(class string, data []byte) (int64, error) {
	if !e.ClassExists(class) {
		return 0, ErrNoClass
	}
	id := e.idgen.Next(class)
	if _, err := e.submit(wal.WALEntry{Op: wal.OpPut, Class: class, ObjectID: id, Data: data}, false); err != nil {
		return 0, err
	}
	return id, nil
}

// GetObject returns the object's data.
func (e *Engine) GetObject(class string, id int64) ([]byte, bool, error) {
	if !e.ClassExists(class) {
		return nil, false, ErrNoClass
	}
	return e.lookup(wal.ObjectKey(class, id))
}

// PutObject replaces an existing object (404 if it does not exist).
func (e *Engine) PutObject(class string, id int64, data []byte) error {
	if !e.ClassExists(class) {
		return ErrNoClass
	}
	_, found, err := e.lookup(wal.ObjectKey(class, id))
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	_, err = e.submit(wal.WALEntry{Op: wal.OpPut, Class: class, ObjectID: id, Data: data}, false)
	return err
}

// DeleteObject removes an object (404 if it does not exist).
func (e *Engine) DeleteObject(class string, id int64) error {
	if !e.ClassExists(class) {
		return ErrNoClass
	}
	_, found, err := e.lookup(wal.ObjectKey(class, id))
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	_, err = e.submit(wal.WALEntry{Op: wal.OpDelete, Class: class, ObjectID: id}, true)
	return err
}

// ListObjects returns objects of a class in ascending id order, merging the
// snapshot with overlay deltas, paginated by limit/offset.
func (e *Engine) ListObjects(class string, limit, offset int) ([]Object, error) {
	if !e.ClassExists(class) {
		return nil, ErrNoClass
	}
	merged := make(map[int64][]byte)

	snap := e.mapped.Acquire()
	err := snap.Scan(wal.ObjectPrefix(class), func(key, val []byte) bool {
		if _, id, ok := wal.DecodeObjectKey(key); ok {
			merged[id] = append([]byte(nil), val...)
		}
		return true
	})
	snap.Release()
	if err != nil {
		return nil, err
	}

	// Apply overlay deltas for this class.
	e.mu.RLock()
	for keyStr, ov := range e.overlay {
		key := []byte(keyStr)
		c, id, ok := wal.DecodeObjectKey(key)
		if !ok || c != class {
			continue
		}
		if ov.deleted {
			delete(merged, id)
		} else {
			merged[id] = append([]byte(nil), ov.data...)
		}
	}
	e.mu.RUnlock()

	ids := make([]int64, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	if offset < 0 {
		offset = 0
	}
	if offset > len(ids) {
		offset = len(ids)
	}
	ids = ids[offset:]
	if limit > 0 && limit < len(ids) {
		ids = ids[:limit]
	}
	out := make([]Object, 0, len(ids))
	for _, id := range ids {
		out = append(out, Object{ID: id, Data: merged[id]})
	}
	return out, nil
}

// ---- Checkpointing & stats ----

// Checkpoint folds pending writes into a new data generation immediately.
func (e *Engine) Checkpoint() error {
	e.ckMu.Lock()
	defer e.ckMu.Unlock()

	newGen, foldedTxID, err := checkpoint.Run(e.cfg.Dir, e.seq, e.mapped)
	if err != nil {
		return fmt.Errorf("engine: checkpoint: %w", err)
	}
	// Prune overlay entries now durably in the new snapshot. Done AFTER the
	// snapshot swap (inside Run) so a key is never absent from both.
	e.mu.Lock()
	for k, ov := range e.overlay {
		if ov.txID <= foldedTxID {
			delete(e.overlay, k)
		}
	}
	e.activeGen = newGen
	e.mu.Unlock()

	// ckMu is already held for the duration of this call.
	e.checkpoints++
	e.lastCheckpnt = time.Now()
	return nil
}

// checkpointLoop triggers checkpoints by WAL size or interval.
func (e *Engine) checkpointLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	lastRun := time.Now()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.mu.RLock()
			pending := len(e.overlay) > 0
			e.mu.RUnlock()
			bySize := e.seq.Offset() >= e.cfg.CheckpointWALBytes
			byTime := pending && time.Since(lastRun) >= e.cfg.CheckpointInterval
			if bySize || byTime {
				if err := e.Checkpoint(); err == nil {
					lastRun = time.Now()
				}
			}
		}
	}
}

// Stats returns current engine counters.
func (e *Engine) Stats() Stats {
	e.mu.RLock()
	overlaySize := len(e.overlay)
	numClasses := len(e.classes)
	gen := e.activeGen
	e.mu.RUnlock()
	e.ckMu.Lock()
	cks := e.checkpoints
	last := e.lastCheckpnt
	e.ckMu.Unlock()
	return Stats{
		LastTxID:     e.seq.LastTxID(),
		WALBytes:     e.seq.Offset(),
		ActiveGen:    gen,
		NumClasses:   numClasses,
		OverlaySize:  overlaySize,
		Checkpoints:  cks,
		LastCheckpnt: last,
	}
}

func validClassName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c == '_' || c == '-' || c == '.' ||
			(c >= '0' && c <= '9') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z')
		if !ok {
			return false
		}
	}
	return true
}
