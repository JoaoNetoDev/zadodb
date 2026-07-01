package wal

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned by Submit after the sequencer has been stopped.
var ErrClosed = errors.New("wal: sequencer closed")

// Sequencer is the single goroutine that appends to the WAL. Callers submit an
// already-marshaled payload; the sequencer assigns a monotonic TxID, frames the
// record, and fsyncs according to the configured policy before acknowledging.
//
// This is the engine's ONLY serialization point on the write path. Because a
// single goroutine owns the file, appends need no lock; TxID assignment is
// therefore naturally race-free.
type Sequencer struct {
	w    *Writer
	mode FsyncMode
	reqs chan *request

	nextTx atomic.Uint64 // last assigned TxID (for stats / reads)

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

type request struct {
	payload []byte
	resp    chan result
}

type result struct {
	txID uint64
	err  error
}

// NewSequencer creates a sequencer over w. initialTxID is the last TxID already
// present durably (recovery supplies this); the first assigned TxID is
// initialTxID+1. queue bounds in-flight submissions.
func NewSequencer(w *Writer, mode FsyncMode, initialTxID uint64, queue int) *Sequencer {
	if queue <= 0 {
		queue = 1024
	}
	s := &Sequencer{
		w:    w,
		mode: mode,
		reqs: make(chan *request, queue),
		done: make(chan struct{}),
	}
	s.nextTx.Store(initialTxID)
	s.wg.Add(1)
	go s.run()
	return s
}

// LastTxID returns the most recently assigned TxID.
func (s *Sequencer) LastTxID() uint64 { return s.nextTx.Load() }

// Offset returns the current WAL end offset.
func (s *Sequencer) Offset() int64 { return s.w.Offset() }

// Submit appends payload to the WAL and blocks until it is durable (per the
// fsync policy), returning the assigned TxID.
func (s *Sequencer) Submit(payload []byte) (uint64, error) {
	req := &request{payload: payload, resp: make(chan result, 1)}
	select {
	case s.reqs <- req:
	case <-s.done:
		return 0, ErrClosed
	}
	res := <-req.resp
	return res.txID, res.err
}

// Close stops the sequencer and closes the underlying writer. In-flight and
// queued submissions receive ErrClosed.
func (s *Sequencer) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	s.wg.Wait()
	return s.w.Close()
}

func (s *Sequencer) run() {
	defer s.wg.Done()
	txIDs := make([]uint64, 0, 256)
	for {
		var first *request
		select {
		case first = <-s.reqs:
		case <-s.done:
			s.drainClosed()
			return
		}

		batch := []*request{first}
		if s.mode.Policy == FsyncGroupCommit {
			batch = s.collectBatch(batch)
		}

		// Assign TxIDs and append every record in the batch.
		txIDs = txIDs[:0]
		var err error
		for _, r := range batch {
			tx := s.nextTx.Add(1)
			txIDs = append(txIDs, tx)
			if err == nil {
				err = s.w.Append(tx, r.payload)
			}
		}
		// A single fsync makes the whole batch durable.
		if err == nil {
			err = s.w.Sync()
		}
		// Deliver the durability result exactly once per request.
		for i, r := range batch {
			r.resp <- result{txID: txIDs[i], err: err}
		}
	}
}

func (s *Sequencer) collectBatch(batch []*request) []*request {
	timer := time.NewTimer(s.mode.Interval)
	defer timer.Stop()
	for len(batch) < s.mode.MaxBatch {
		select {
		case r := <-s.reqs:
			batch = append(batch, r)
		case <-timer.C:
			return batch
		case <-s.done:
			return batch
		}
	}
	return batch
}

func (s *Sequencer) drainClosed() {
	for {
		select {
		case r := <-s.reqs:
			r.resp <- result{err: ErrClosed}
		default:
			return
		}
	}
}
