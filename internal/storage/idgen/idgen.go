// Package idgen assigns per-class auto-increment object ids.
//
// Ids are not persisted separately: at boot the engine replays the tree and WAL
// and calls Observe for every id it sees, so the generator resumes just past
// the highest id already durable. This keeps a single source of truth (the
// data itself) rather than a second counter that could drift after a crash.
package idgen

import "sync"

// Generator hands out monotonically increasing ids per class. It is safe for
// concurrent use.
type Generator struct {
	mu       sync.Mutex
	counters map[string]int64
}

// New returns an empty generator (every class starts at 0, so the first Next is 1).
func New() *Generator {
	return &Generator{counters: make(map[string]int64)}
}

// Next returns the next id for class (starting at 1).
func (g *Generator) Next(class string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counters[class]++
	return g.counters[class]
}

// Observe raises class's counter to at least id. Used during recovery so ids
// are never reused after a restart.
func (g *Generator) Observe(class string, id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if id > g.counters[class] {
		g.counters[class] = id
	}
}

// Current returns the last id handed out for class (0 if none).
func (g *Generator) Current(class string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.counters[class]
}

// Drop forgets a class's counter (used when a class is dropped).
func (g *Generator) Drop(class string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.counters, class)
}
