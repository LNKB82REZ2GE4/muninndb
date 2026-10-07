package storage

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/scrypster/muninndb/internal/provenance"
)

const provenanceChanDepth = 32768 // was 8192 — absorbs burst at 63k writes/sec

type provenanceJob struct {
	wsPrefix [8]byte
	id       ULID
	entry    provenance.ProvenanceEntry
}

// provenanceWorker fans out provenance appends across NumCPU goroutines.
// Each Append in provenance.Store is now a single O(1) Pebble Set (no
// read-modify-write), so concurrent goroutines are safe to run in parallel
// without per-ID coordination. The global atomic sequence in the key format
// ensures uniqueness even under sub-nanosecond timestamp collisions.
type provenanceWorker struct {
	store   *provenance.Store
	pending chan provenanceJob
	dropped atomic.Int64
	wg      sync.WaitGroup
	done    chan struct{}

	// inFlight counts submitted-but-unfinished jobs PER VAULT, guarded by
	// inFlightMu; drained is broadcast whenever a vault's count reaches zero.
	//
	// This was a single process-wide sync.WaitGroup. ClearVault Waited on it
	// while writers in every other vault kept calling Add(1) — and a positive
	// Add from zero concurrent with Wait is the reuse the WaitGroup contract
	// forbids, so ClearVault panicked ("sync: WaitGroup is reused before
	// previous Wait has returned") within a handful of clears under ordinary
	// concurrent writes. A global count also meant a busy vault could starve a
	// clear of an unrelated one. Pinned by
	// TestClearVault_ConcurrentWritesToOtherVaultDoNotPanic and
	// TestProvenanceWorker_DrainVaultIgnoresOtherVaults.
	inFlightMu sync.Mutex
	drained    *sync.Cond
	inFlight   map[[8]byte]int
}

func newProvenanceWorker(store *provenance.Store) *provenanceWorker {
	n := runtime.NumCPU()
	w := &provenanceWorker{
		store:    store,
		pending:  make(chan provenanceJob, provenanceChanDepth),
		done:     make(chan struct{}),
		inFlight: make(map[[8]byte]int),
	}
	w.drained = sync.NewCond(&w.inFlightMu)
	w.wg.Add(n)
	for range n {
		go func() {
			defer w.wg.Done()
			w.run()
		}()
	}
	return w
}

// Submit enqueues a provenance append. Non-blocking — drops and warns if full.
// Recovers from "send on closed channel" during graceful shutdown (e.g., when
// the decay worker processes a final batch after Close() has been called).
// The in-flight count is taken before the channel send so that DrainVault can
// never observe a zero count between enqueue and processing.
func (w *provenanceWorker) Submit(wsPrefix [8]byte, id ULID, entry provenance.ProvenanceEntry) {
	w.acquire(wsPrefix)
	sent := false
	func() {
		defer func() { recover() }() // safe against send-on-closed-channel during shutdown
		select {
		case w.pending <- provenanceJob{wsPrefix: wsPrefix, id: id, entry: entry}:
			sent = true
		default:
		}
	}()
	if !sent {
		w.release(wsPrefix) // not enqueued — release immediately
		n := w.dropped.Add(1)
		if n&(n-1) == 0 { // log at powers of 2 to rate-limit slog I/O
			slog.Warn("storage: provenance worker full, entries dropped", "total_dropped", n)
		}
	}
}

// safeAppend calls store.Append but recovers from panics (e.g., "pebble: closed"
// during test teardown when db.Close() races with the worker).
func (w *provenanceWorker) safeAppend(ctx context.Context, job provenanceJob) {
	defer func() { recover() }()
	if err := w.store.Append(ctx, job.wsPrefix, [16]byte(job.id), job.entry); err != nil {
		slog.Warn("storage: provenance append failed", "engram_id", job.id, "err", err)
	}
}

// run drains the shared channel until it is closed (on Stop).
// The in-flight count is taken in Submit before the job enters the channel,
// so we only need to release it here after processing completes.
func (w *provenanceWorker) run() {
	ctx := context.Background()
	for job := range w.pending {
		w.safeAppend(ctx, job)
		w.release(job.wsPrefix)
	}
}

func (w *provenanceWorker) acquire(ws [8]byte) {
	w.inFlightMu.Lock()
	w.inFlight[ws]++
	w.inFlightMu.Unlock()
}

func (w *provenanceWorker) release(ws [8]byte) {
	w.inFlightMu.Lock()
	if n := w.inFlight[ws] - 1; n > 0 {
		w.inFlight[ws] = n
	} else {
		delete(w.inFlight, ws)
		w.drained.Broadcast()
	}
	w.inFlightMu.Unlock()
}

// DrainVault waits until no provenance append for ws is in flight. Used by
// ClearVault so no 0x16 key for the cleared vault lands after its range
// tombstones. Appends for other vaults neither block it nor race it.
func (w *provenanceWorker) DrainVault(ws [8]byte) {
	w.inFlightMu.Lock()
	for w.inFlight[ws] > 0 {
		w.drained.Wait()
	}
	w.inFlightMu.Unlock()
}

// Drain waits until no provenance append for any vault is in flight. Test
// helper semantics: under continuous writes it may not return promptly.
func (w *provenanceWorker) Drain() {
	w.inFlightMu.Lock()
	for len(w.inFlight) > 0 {
		w.drained.Wait()
	}
	w.inFlightMu.Unlock()
}

// Close drains the queue and waits for all worker goroutines to exit.
// Must be called before db.Close().
func (w *provenanceWorker) Close() {
	close(w.pending)
	w.wg.Wait()
	close(w.done)
}
