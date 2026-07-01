package wal

import "time"

// FsyncPolicy selects how aggressively the sequencer fsyncs the WAL.
type FsyncPolicy uint8

const (
	// FsyncPerCommit fsyncs after every single record before acknowledging the
	// writer. This is the safe default: a durably acknowledged write is never
	// lost, at the cost of one fsync per commit.
	FsyncPerCommit FsyncPolicy = iota
	// FsyncGroupCommit coalesces concurrent commits into a batch sharing a
	// single fsync, trading a tiny durability window for much higher throughput
	// under concurrent load. All writers in a batch are acknowledged together,
	// only after the shared fsync succeeds.
	FsyncGroupCommit
)

func (p FsyncPolicy) String() string {
	switch p {
	case FsyncPerCommit:
		return "per-commit"
	case FsyncGroupCommit:
		return "group-commit"
	default:
		return "unknown"
	}
}

// FsyncMode fully describes the fsync behaviour.
type FsyncMode struct {
	Policy   FsyncPolicy
	Interval time.Duration // group-commit: max time to wait accumulating a batch
	MaxBatch int           // group-commit: max records per batch
}

// DefaultFsyncMode is the safe, per-commit default.
func DefaultFsyncMode() FsyncMode {
	return FsyncMode{Policy: FsyncPerCommit}
}

// GroupCommitMode returns a group-commit configuration with sane bounds.
func GroupCommitMode(interval time.Duration, maxBatch int) FsyncMode {
	if interval <= 0 {
		interval = 2 * time.Millisecond
	}
	if maxBatch <= 0 {
		maxBatch = 256
	}
	return FsyncMode{Policy: FsyncGroupCommit, Interval: interval, MaxBatch: maxBatch}
}
