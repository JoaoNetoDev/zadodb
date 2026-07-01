package wal

import (
	"errors"
	"fmt"
	"os"
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
	// w is the active WAL writer. It is reassigned by doRotate (on the run
	// goroutine) and read concurrently by Offset/Close, so it is held behind an
	// atomic pointer.
	w    atomic.Pointer[Writer]
	mode FsyncMode
	reqs chan *request
	rot  chan *rotateReq

	nextTx atomic.Uint64 // last assigned TxID (for stats / reads)

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

type rotateReq struct {
	retiredPath string
	done        chan error
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
		mode: mode,
		reqs: make(chan *request, queue),
		rot:  make(chan *rotateReq),
		done: make(chan struct{}),
	}
	s.w.Store(w)
	s.nextTx.Store(initialTxID)
	s.wg.Add(1)
	go s.run()
	return s
}

// LastTxID returns the most recently assigned TxID.
func (s *Sequencer) LastTxID() uint64 { return s.nextTx.Load() }

// Offset returns the current WAL end offset.
func (s *Sequencer) Offset() int64 { return s.w.Load().Offset() }

// Submit appends payload to the WAL and blocks until it is durable (per the
// fsync policy), returning the assigned TxID.
func (s *Sequencer) Submit(payload []byte) (uint64, error) {
	req := &request{payload: payload, resp: make(chan result, 1)}
	select {
	case s.reqs <- req:
	case <-s.done:
		return 0, ErrClosed
	}
	// Prefer a real answer if one is already available, so a durably-committed
	// write is never misreported as closed during shutdown.
	select {
	case res := <-req.resp:
		return res.txID, res.err
	default:
	}
	// Also watch done while awaiting the reply: if the sequencer is closing, the
	// request may have been enqueued into the buffer just as the run goroutine
	// exited, so nobody will ever answer it. done unblocks us with ErrClosed.
	select {
	case res := <-req.resp:
		return res.txID, res.err
	case <-s.done:
		return 0, ErrClosed
	}
}

// Rotate cuts the WAL at a clean record boundary: the current log is fsynced
// and renamed to retiredPath, and a fresh empty log is opened at the original
// path. Records already written are now entirely in retiredPath; all subsequent
// submissions go to the fresh log. TxIDs continue monotonically across the cut.
//
// This is how a checkpoint captures exactly the set of records to fold into a
// new data generation without racing concurrent writers.
func (s *Sequencer) Rotate(retiredPath string) error {
	req := &rotateReq{retiredPath: retiredPath, done: make(chan error, 1)}
	select {
	case s.rot <- req:
	case <-s.done:
		return ErrClosed
	}
	return <-req.done
}

func (s *Sequencer) doRotate(req *rotateReq) {
	cur := s.w.Load()
	if err := cur.Sync(); err != nil {
		req.done <- err
		return
	}
	active := cur.Path()
	if err := cur.Close(); err != nil {
		req.done <- err
		return
	}
	if err := os.Rename(active, req.retiredPath); err != nil {
		req.done <- fmt.Errorf("wal: rotate rename: %w", err)
		return
	}
	nw, err := OpenWriter(active)
	if err != nil {
		// The old writer is already closed and renamed; try to restore it so
		// the sequencer is not left permanently wedged.
		if rerr := os.Rename(req.retiredPath, active); rerr == nil {
			if restored, oerr := OpenWriter(active); oerr == nil {
				s.w.Store(restored)
			}
		}
		req.done <- fmt.Errorf("wal: rotate reopen: %w", err)
		return
	}
	s.w.Store(nw)
	req.done <- nil
}

// Close stops the sequencer and closes the underlying writer. In-flight and
// queued submissions receive ErrClosed.
func (s *Sequencer) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	s.wg.Wait()
	return s.w.Load().Close()
}

func (s *Sequencer) run() {
	defer s.wg.Done()
	txIDs := make([]uint64, 0, 256)
	for {
		var first *request
		select {
		case first = <-s.reqs:
		case req := <-s.rot:
			s.doRotate(req)
			continue
		case <-s.done:
			s.drainClosed()
			return
		}

		batch := []*request{first}
		if s.mode.Policy == FsyncGroupCommit {
			batch = s.collectBatch(batch)
		}

		// Assign TxIDs and append every record in the batch to a single writer
		// (loaded once so a concurrent rotation cannot split a batch).
		w := s.w.Load()
		txIDs = txIDs[:0]
		var err error
		for _, r := range batch {
			tx := s.nextTx.Add(1)
			txIDs = append(txIDs, tx)
			if err == nil {
				err = w.Append(tx, r.payload)
			}
		}
		// A single fsync makes the whole batch durable.
		if err == nil {
			err = w.Sync()
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
