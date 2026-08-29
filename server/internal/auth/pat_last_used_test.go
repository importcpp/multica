package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// fakeBatchWriter records the ids passed to each flush and can be told to fail
// or panic. Concurrency-safe.
type fakeBatchWriter struct {
	mu       sync.Mutex
	batches  [][]pgtype.UUID
	failNext bool
	panicNow bool
	calls    int
}

func (f *fakeBatchWriter) UpdatePersonalAccessTokensLastUsed(ctx context.Context, ids []pgtype.UUID) error {
	f.mu.Lock()
	f.calls++
	fail, panicNow := f.failNext, f.panicNow
	cp := append([]pgtype.UUID(nil), ids...)
	f.mu.Unlock()
	if panicNow {
		panic("boom")
	}
	if fail {
		return errors.New("db down")
	}
	f.mu.Lock()
	f.batches = append(f.batches, cp)
	f.mu.Unlock()
	return nil
}

func (f *fakeBatchWriter) flushedIDs() []pgtype.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []pgtype.UUID
	for _, b := range f.batches {
		out = append(out, b...)
	}
	return out
}

func uid(b byte) pgtype.UUID {
	var u pgtype.UUID
	u.Valid = true
	for i := range u.Bytes {
		u.Bytes[i] = b
	}
	return u
}

func newTestRecorder(w lastUsedBatchWriter) *BatchedPATLastUsedRecorder {
	return &BatchedPATLastUsedRecorder{
		queries:       w,
		flushInterval: time.Hour, // we drive flushOnce directly
		maxPending:    4,
		maxBatch:      2,
		flushBudget:   time.Second,
		shutdownDur:   time.Second,
		pending:       make(map[pgtype.UUID]struct{}),
		stopped:       make(chan struct{}),
		done:          make(chan struct{}),
		metrics:       atomicNoopMetrics{},
	}
}

func TestRecord_DedupsAndFlushes(t *testing.T) {
	w := &fakeBatchWriter{}
	r := newTestRecorder(w)
	r.Record(uid(1))
	r.Record(uid(1)) // dup — must not add a second slot
	r.Record(uid(2))
	if got := len(r.pending); got != 2 {
		t.Fatalf("pending = %d, want 2 (dup merged)", got)
	}
	r.flushOnce(context.Background(), time.Second)
	if got := len(w.flushedIDs()); got != 2 {
		t.Fatalf("flushed %d ids, want 2", got)
	}
	if len(r.pending) != 0 {
		t.Fatalf("pending not drained after flush")
	}
}

func TestRecord_FullDropsNewButDedupsExisting(t *testing.T) {
	w := &fakeBatchWriter{}
	r := newTestRecorder(w) // maxPending=4
	for i := byte(1); i <= 4; i++ {
		r.Record(uid(i))
	}
	// Map full. An existing id must still be treated as dedup, not drop.
	r.Record(uid(1)) // dedup
	r.Record(uid(9)) // drop
	if len(r.pending) != 4 {
		t.Fatalf("pending = %d, want 4", len(r.pending))
	}
}

func TestFlush_ChunksByMaxBatch(t *testing.T) {
	w := &fakeBatchWriter{}
	r := newTestRecorder(w) // maxBatch=2
	for i := byte(1); i <= 4; i++ {
		r.Record(uid(i))
	}
	r.flushOnce(context.Background(), time.Second)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.batches) != 2 {
		t.Fatalf("got %d chunks, want 2 (4 ids / maxBatch 2)", len(w.batches))
	}
}

func TestFlush_DBErrorStopsRoundNoPanic(t *testing.T) {
	w := &fakeBatchWriter{failNext: true}
	r := newTestRecorder(w)
	for i := byte(1); i <= 4; i++ {
		r.Record(uid(i))
	}
	r.flushOnce(context.Background(), time.Second) // must not panic
	if len(w.flushedIDs()) != 0 {
		t.Fatalf("no ids should have been recorded on failure")
	}
}

func TestFlush_PanicRecoveredWorkerSurvives(t *testing.T) {
	w := &fakeBatchWriter{panicNow: true}
	r := newTestRecorder(w)
	r.Record(uid(1))
	r.flushOnce(context.Background(), time.Second) // recovers, does not crash

	// Next round with a healthy writer still works.
	w.mu.Lock()
	w.panicNow = false
	w.mu.Unlock()
	r.Record(uid(2))
	r.flushOnce(context.Background(), time.Second)
	if len(w.flushedIDs()) != 1 {
		t.Fatalf("worker did not recover for the next round")
	}
}

func TestStop_FinalFlushUsesIndependentContext(t *testing.T) {
	w := &fakeBatchWriter{}
	r := newTestRecorder(w)

	// Simulate main.go: cancel the worker ctx, THEN Stop. The final flush
	// must still write despite the worker ctx being dead.
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	r.Record(uid(7))
	cancel() // sweepCancel() equivalent — worker ctx now cancelled
	r.Stop() // final flush must not derive from the cancelled ctx

	if got := len(w.flushedIDs()); got != 1 {
		t.Fatalf("final flush wrote %d ids, want 1 (independent ctx)", got)
	}
}

func TestStop_Idempotent(t *testing.T) {
	w := &fakeBatchWriter{}
	r := newTestRecorder(w)
	r.Stop()
	r.Stop() // must not panic / double-close
}

func TestNoopRecorder(t *testing.T) {
	var r PATLastUsedRecorder = NoopPATLastUsedRecorder{}
	r.Record(uid(1)) // no panic, no state
}

func TestRecord_ConcurrentDedup(t *testing.T) {
	w := &fakeBatchWriter{}
	r := newTestRecorder(w)
	r.maxPending = 1000
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Record(uid(42)) }()
	}
	wg.Wait()
	if len(r.pending) != 1 {
		t.Fatalf("concurrent dup: pending = %d, want 1", len(r.pending))
	}
}
