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
	"math"
	"sort"
	"sync"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/storage/checkpoint"
	"github.com/JoaoNetoDev/zadodb/internal/storage/idgen"
	"github.com/JoaoNetoDev/zadodb/internal/storage/layout"
	"github.com/JoaoNetoDev/zadodb/internal/storage/mvcc"
	"github.com/JoaoNetoDev/zadodb/internal/storage/recovery"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
	"github.com/vmihailenco/msgpack/v5"
)

// Sentinel errors surfaced to the API layer.
var (
	ErrClassExists   = errors.New("class already exists")
	ErrNoClass       = errors.New("class does not exist")
	ErrClassNotEmpty = errors.New("class is not empty")
	ErrNotFound      = errors.New("object not found")
	ErrInvalidName   = errors.New("invalid class name")
	ErrRelExists     = errors.New("relationship already exists")
	ErrNoRel         = errors.New("relationship does not exist")
)

// Relationship is a foreign key from a class's local field to a target class's
// remote field, registered once and used to resolve joins in queries. Name
// defaults to ToClass and is how queries reference the relation (e.g. the
// "municipio" in eq.municipio.nome).
type Relationship struct {
	Name        string `msgpack:"name" json:"name"`
	LocalField  string `msgpack:"local" json:"localField"`
	ToClass     string `msgpack:"to" json:"toClass"`
	RemoteField string `msgpack:"remote" json:"remoteField"`
}

// Config configures the storage engine.
type Config struct {
	Dir                  string
	Fsync                wal.FsyncMode
	CheckpointWALBytes   int64         // checkpoint when the WAL grows past this (0 = default 64MiB)
	CheckpointInterval   time.Duration // periodic checkpoint if writes are pending (0 = default 5m)
	CheckpointManual     bool          // disable automatic checkpoints; only POST /v1/checkpoint (or Checkpoint()) folds
	CheckpointMaxOverlay int           // force a checkpoint when overlay entries exceed this, even in manual mode (0 = no cap)
	QueueDepth           int           // sequencer submission queue depth
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

// scopeKey is the in-memory identity of a class within a project. Neither
// project nor class names may contain 0x00 (enforced by validName), so the
// separator is unambiguous. Used to key the class set and the id generator so
// the same class name in two projects stays independent.
func scopeKey(project, class string) string { return wal.ScopeKey(project, class) }

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
	rels      map[string]map[string]Relationship // scopeKey(project,fromClass) -> name -> rel
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
		rels:      make(map[string]map[string]Relationship),
		activeGen: res.ActiveGen,
		stopCh:    make(chan struct{}),
	}
	if err := e.loadClasses(); err != nil {
		e.Close()
		return nil, err
	}
	if err := e.loadRels(); err != nil {
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
		if project, name, ok := wal.DecodeClassKey(key); ok {
			e.classes[scopeKey(project, name)] = struct{}{}
		}
		return true
	})
}

// loadRels seeds the in-memory relationship set from relationship-definition
// keys already stored in the active generation.
func (e *Engine) loadRels() error {
	snap := e.mapped.Acquire()
	defer snap.Release()
	return snap.Scan([]byte{0x03}, func(key, val []byte) bool {
		if project, class, name, ok := wal.DecodeRelKey(key); ok {
			var rel Relationship
			if msgpack.Unmarshal(val, &rel) == nil {
				rel.Name = name
				e.putRel(project, class, rel)
			}
		}
		return true
	})
}

// putRel stores a relationship in the in-memory map (caller holds no lock at
// boot; guarded by e.mu elsewhere).
func (e *Engine) putRel(project, class string, rel Relationship) {
	sk := scopeKey(project, class)
	if e.rels[sk] == nil {
		e.rels[sk] = make(map[string]Relationship)
	}
	e.rels[sk][rel.Name] = rel
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
			e.classes[scopeKey(r.Entry.Project, r.Entry.Class)] = struct{}{}
		case wal.OpDropClass:
			e.overlay[key] = overlayVal{txID: r.TxID, deleted: true}
			delete(e.classes, scopeKey(r.Entry.Project, r.Entry.Class))
		case wal.OpCreateRel:
			e.overlay[key] = overlayVal{txID: r.TxID, data: r.Entry.Data}
			var rel Relationship
			if msgpack.Unmarshal(r.Entry.Data, &rel) == nil {
				rel.Name = r.Entry.Name
				e.putRel(r.Entry.Project, r.Entry.Class, rel)
			}
		case wal.OpDropRel:
			e.overlay[key] = overlayVal{txID: r.TxID, deleted: true}
			if m := e.rels[scopeKey(r.Entry.Project, r.Entry.Class)]; m != nil {
				delete(m, r.Entry.Name)
			}
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

// ---- Projects ----

// ListProjects returns the distinct project names that currently hold at least
// one class, sorted. The default project "" is included when it has classes.
func (e *Engine) ListProjects() []string {
	e.mu.RLock()
	seen := make(map[string]struct{})
	for k := range e.classes {
		project, _ := splitScopeKey(k)
		seen[project] = struct{}{}
	}
	e.mu.RUnlock()
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// splitScopeKey reverses scopeKey.
func splitScopeKey(k string) (project, class string) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0x00 {
			return k[:i], k[i+1:]
		}
	}
	return "", k
}

// ---- Classes ----

// CreateClass registers a new class within a project (project "" is the default).
func (e *Engine) CreateClass(project, name string) error {
	if !validName(name) || !validProject(project) {
		return ErrInvalidName
	}
	sk := scopeKey(project, name)
	e.mu.RLock()
	_, exists := e.classes[sk]
	e.mu.RUnlock()
	if exists {
		return ErrClassExists
	}
	if _, err := e.submit(wal.WALEntry{Op: wal.OpCreateClass, Project: project, Class: name}, false); err != nil {
		return err
	}
	e.mu.Lock()
	e.classes[sk] = struct{}{}
	e.mu.Unlock()
	return nil
}

// ListClasses returns the existing class names in a project, sorted.
func (e *Engine) ListClasses(project string) []string {
	e.mu.RLock()
	out := make([]string, 0, len(e.classes))
	for k := range e.classes {
		p, c := splitScopeKey(k)
		if p == project {
			out = append(out, c)
		}
	}
	e.mu.RUnlock()
	sort.Strings(out)
	return out
}

// ClassExists reports whether a class exists in a project.
func (e *Engine) ClassExists(project, name string) bool {
	e.mu.RLock()
	_, ok := e.classes[scopeKey(project, name)]
	e.mu.RUnlock()
	return ok
}

// DropClass removes an empty class from a project.
func (e *Engine) DropClass(project, name string) error {
	if !e.ClassExists(project, name) {
		return ErrNoClass
	}
	// Reject if any object of the class remains.
	objs, err := e.ListObjects(project, name, 1, 0)
	if err != nil {
		return err
	}
	if len(objs) > 0 {
		return ErrClassNotEmpty
	}
	if _, err := e.submit(wal.WALEntry{Op: wal.OpDropClass, Project: project, Class: name}, true); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.classes, scopeKey(project, name))
	e.mu.Unlock()
	e.idgen.Drop(scopeKey(project, name))
	return nil
}

// ---- Relationships ----

// CreateRelationship registers a foreign key from fromClass.LocalField to
// rel.ToClass.RemoteField. rel.Name defaults to rel.ToClass and is how queries
// reference the relation. Both classes must already exist in the project.
func (e *Engine) CreateRelationship(project, fromClass string, rel Relationship) error {
	if rel.Name == "" {
		rel.Name = rel.ToClass
	}
	if !validName(fromClass) || !validName(rel.Name) || !validName(rel.ToClass) ||
		rel.LocalField == "" || rel.RemoteField == "" {
		return ErrInvalidName
	}
	if !e.ClassExists(project, fromClass) || !e.ClassExists(project, rel.ToClass) {
		return ErrNoClass
	}
	sk := scopeKey(project, fromClass)
	e.mu.RLock()
	_, exists := e.rels[sk][rel.Name]
	e.mu.RUnlock()
	if exists {
		return ErrRelExists
	}
	spec, err := msgpack.Marshal(rel)
	if err != nil {
		return err
	}
	if _, err := e.submit(wal.WALEntry{Op: wal.OpCreateRel, Project: project, Class: fromClass, Name: rel.Name, Data: spec}, false); err != nil {
		return err
	}
	e.mu.Lock()
	e.putRel(project, fromClass, rel)
	e.mu.Unlock()
	return nil
}

// DropRelationship removes a relationship by name.
func (e *Engine) DropRelationship(project, fromClass, name string) error {
	sk := scopeKey(project, fromClass)
	e.mu.RLock()
	_, ok := e.rels[sk][name]
	e.mu.RUnlock()
	if !ok {
		return ErrNoRel
	}
	if _, err := e.submit(wal.WALEntry{Op: wal.OpDropRel, Project: project, Class: fromClass, Name: name}, true); err != nil {
		return err
	}
	e.mu.Lock()
	if m := e.rels[sk]; m != nil {
		delete(m, name)
	}
	e.mu.Unlock()
	return nil
}

// ListRelationships returns the relationships declared on a class, sorted by name.
func (e *Engine) ListRelationships(project, fromClass string) []Relationship {
	e.mu.RLock()
	m := e.rels[scopeKey(project, fromClass)]
	out := make([]Relationship, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	e.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Relationship returns a single relationship of a class by name.
func (e *Engine) Relationship(project, fromClass, name string) (Relationship, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.rels[scopeKey(project, fromClass)][name]
	return r, ok
}

// ---- Objects ----

// CreateObject assigns a new id and stores data, returning the id.
func (e *Engine) CreateObject(project, class string, data []byte) (int64, error) {
	if !e.ClassExists(project, class) {
		return 0, ErrNoClass
	}
	id := e.idgen.Next(scopeKey(project, class))
	if _, err := e.submit(wal.WALEntry{Op: wal.OpPut, Project: project, Class: class, ObjectID: id, Data: data}, false); err != nil {
		return 0, err
	}
	return id, nil
}

// CreateObjectsBulk atomically stores many objects in a single WAL record
// (one fsync for the whole batch) and returns their assigned ids in order.
//
// Atomicity: the batch is one WAL record, so a crash either leaves the complete
// record (all objects durable) or a torn record (none applied) — never a
// partial batch. A successful return means every object is durable; on error or
// a crash without a successful return, the batch may be entirely absent (retry).
func (e *Engine) CreateObjectsBulk(project, class string, datas [][]byte) ([]int64, error) {
	if !e.ClassExists(project, class) {
		return nil, ErrNoClass
	}
	if len(datas) == 0 {
		return []int64{}, nil
	}
	ids := make([]int64, len(datas))
	subs := make([]wal.WALEntry, len(datas))
	ts := time.Now().UnixNano()
	sk := scopeKey(project, class)
	for i, d := range datas {
		id := e.idgen.Next(sk)
		ids[i] = id
		subs[i] = wal.WALEntry{Op: wal.OpPut, Project: project, Class: class, ObjectID: id, Data: d, Timestamp: ts}
	}

	batch := wal.WALEntry{Op: wal.OpBatch, Timestamp: ts, Sub: subs}
	payload, err := batch.Marshal() // marshaled off the sequencer's critical path
	if err != nil {
		return nil, err
	}
	txID, err := e.seq.Submit(payload)
	if err != nil {
		return nil, err
	}

	// Publish all sub-mutations to the overlay together, so readers see the
	// whole batch atomically.
	e.mu.Lock()
	for i := range subs {
		e.overlay[string(subs[i].Key())] = overlayVal{txID: txID, data: subs[i].Data}
	}
	e.mu.Unlock()
	return ids, nil
}

// GetObject returns the object's data.
func (e *Engine) GetObject(project, class string, id int64) ([]byte, bool, error) {
	if !e.ClassExists(project, class) {
		return nil, false, ErrNoClass
	}
	return e.lookup(wal.ObjectKey(project, class, id))
}

// PutObject replaces an existing object (404 if it does not exist).
func (e *Engine) PutObject(project, class string, id int64, data []byte) error {
	if !e.ClassExists(project, class) {
		return ErrNoClass
	}
	_, found, err := e.lookup(wal.ObjectKey(project, class, id))
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	_, err = e.submit(wal.WALEntry{Op: wal.OpPut, Project: project, Class: class, ObjectID: id, Data: data}, false)
	return err
}

// DeleteObject removes an object (404 if it does not exist).
func (e *Engine) DeleteObject(project, class string, id int64) error {
	if !e.ClassExists(project, class) {
		return ErrNoClass
	}
	_, found, err := e.lookup(wal.ObjectKey(project, class, id))
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	_, err = e.submit(wal.WALEntry{Op: wal.OpDelete, Project: project, Class: class, ObjectID: id}, true)
	return err
}

// ListObjects returns objects of a class in ascending id order, merging the
// snapshot with overlay deltas, paginated by limit/offset.
func (e *Engine) ListObjects(project, class string, limit, offset int) ([]Object, error) {
	return e.QueryObjects(project, class, nil, limit, offset)
}

// prefixEnd returns the smallest key strictly greater than every key with the
// given prefix (the exclusive upper bound of the prefix range), or nil if the
// prefix is all 0xFF (range runs to the end of the keyspace).
func prefixEnd(p []byte) []byte {
	end := append([]byte(nil), p...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// QueryObjects returns objects of a class in ascending id order (offset/limit
// pagination), optionally filtered by match. It is QueryPage with after == 0.
func (e *Engine) QueryObjects(project, class string, match func(stored []byte) (bool, error), limit, offset int) ([]Object, error) {
	return e.QueryPage(project, class, match, limit, offset, 0)
}

// QueryPage streams a class in ascending id order, merging the mmap snapshot
// with the in-memory overlay on the fly, applies match, and paginates.
//
// Streaming (not materializing the class): the snapshot is scanned in key order
// and merged with the (small) overlay deltas as two sorted runs, so peak memory
// is the returned page plus the overlay slice — not the whole class. This is
// what keeps large classes and joins from exhausting RAM.
//
// Keyset pagination: when after > 0, the scan SEEKS to ids strictly greater than
// after (O(page) via B+Tree range), instead of walking and discarding the first
// `offset` rows (O(offset)). Prefer after for deep pagination over large classes.
//
// There is still no secondary index, so match is evaluated per object; with a
// filter the scan is O(class size). Without a filter, keyset paging is O(page).
func (e *Engine) QueryPage(project, class string, match func(stored []byte) (bool, error), limit, offset int, after int64) ([]Object, error) {
	if !e.ClassExists(project, class) {
		return nil, ErrNoClass
	}
	if offset < 0 {
		offset = 0
	}

	// Overlay deltas for this class with id > after, sorted ascending by id.
	type ovDelta struct {
		id      int64
		data    []byte
		deleted bool
	}
	var ovl []ovDelta
	e.mu.RLock()
	for keyStr, ov := range e.overlay {
		p, c, id, ok := wal.DecodeObjectKey([]byte(keyStr))
		if !ok || p != project || c != class || id <= after {
			continue
		}
		ovl = append(ovl, ovDelta{id: id, data: ov.data, deleted: ov.deleted})
	}
	e.mu.RUnlock()
	sort.Slice(ovl, func(i, j int) bool { return ovl[i].id < ovl[j].id })

	prefix := wal.ObjectPrefix(project, class)
	lo := prefix
	if after > 0 {
		// Smallest key strictly greater than the `after` object's key.
		lo = append(wal.ObjectKey(project, class, after), 0x00)
	}
	hi := prefixEnd(prefix)

	var out []Object
	var scanErr error
	skipped := 0
	ovIdx := 0

	// emit applies the matcher, offset and limit. Returns false to stop.
	emit := func(id int64, data []byte) bool {
		if limit > 0 && len(out) >= limit {
			return false
		}
		if match != nil {
			ok, err := match(data)
			if err != nil {
				scanErr = err
				return false
			}
			if !ok {
				return true
			}
		}
		if skipped < offset {
			skipped++
			return true
		}
		out = append(out, Object{ID: id, Data: append([]byte(nil), data...)})
		return !(limit > 0 && len(out) >= limit)
	}

	// flushOverlay emits pending overlay deltas with id below (or at) bound.
	flushOverlay := func(bound int64, inclusive bool) bool {
		for ovIdx < len(ovl) {
			o := ovl[ovIdx]
			if o.id > bound || (o.id == bound && !inclusive) {
				break
			}
			ovIdx++
			if o.deleted {
				continue
			}
			if !emit(o.id, o.data) {
				return false
			}
		}
		return true
	}

	snap := e.mapped.Acquire()
	err := snap.ScanRange(lo, hi, func(key, val []byte) bool {
		p, c, id, ok := wal.DecodeObjectKey(key)
		if !ok || p != project || c != class {
			return true // defensive: skip foreign keys sharing the byte-prefix
		}
		if !flushOverlay(id, false) { // overlay ids strictly less than id
			return false
		}
		if ovIdx < len(ovl) && ovl[ovIdx].id == id {
			o := ovl[ovIdx] // overlay overrides the snapshot value
			ovIdx++
			if o.deleted {
				return true
			}
			return emit(id, o.data)
		}
		return emit(id, val)
	})
	snap.Release()
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}
	// Overlay ids beyond the snapshot tail (newly inserted, not yet checkpointed).
	flushOverlay(math.MaxInt64, true)
	if scanErr != nil {
		return nil, scanErr
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

// checkpointLoop triggers checkpoints by WAL size or interval. In manual mode
// only the overlay safety cap (CheckpointMaxOverlay) can trigger a fold, so a
// long bulk load cannot grow the in-memory overlay without bound and OOM.
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
			overlaySize := len(e.overlay)
			e.mu.RUnlock()

			byCap := e.cfg.CheckpointMaxOverlay > 0 && overlaySize >= e.cfg.CheckpointMaxOverlay
			trigger := byCap
			if !e.cfg.CheckpointManual {
				bySize := e.seq.Offset() >= e.cfg.CheckpointWALBytes
				byTime := overlaySize > 0 && time.Since(lastRun) >= e.cfg.CheckpointInterval
				trigger = trigger || bySize || byTime
			}
			if trigger {
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

func validName(name string) bool {
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

// validProject accepts the default project ("") or any valid name. The empty
// project uses the legacy key layout, so it must remain always allowed.
func validProject(project string) bool {
	return project == "" || validName(project)
}
