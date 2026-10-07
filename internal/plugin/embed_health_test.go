package plugin

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmbedHealth_StateTransitions(t *testing.T) {
	h := NewEmbedHealth("embed-test")
	clock := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return clock }

	if got := h.Snapshot().State; got != EmbedHealthUnknown {
		t.Fatalf("fresh tracker state = %q, want unknown", got)
	}

	h.RecordSuccess()
	if s := h.Snapshot(); s.State != EmbedHealthOK || s.LastOK == nil || s.DegradedSince != nil {
		t.Fatalf("after success: %+v, want ok with last_ok and no degraded_since", s)
	}

	clock = clock.Add(10 * time.Second)
	firstFail := clock
	h.RecordFailure(errors.New("401 Missing Bearer token"))
	clock = clock.Add(10 * time.Second)
	h.RecordFailure(errors.New("401 Missing Bearer token"))
	s := h.Snapshot()
	if s.State != EmbedHealthDegraded || !s.Degraded() {
		t.Fatalf("after failures: state %q, want degraded", s.State)
	}
	if s.ConsecutiveFailures != 2 || s.TotalFailures != 2 {
		t.Fatalf("failure counts = %d/%d, want 2/2", s.ConsecutiveFailures, s.TotalFailures)
	}
	if s.DegradedSince == nil || !s.DegradedSince.Equal(firstFail) {
		t.Fatalf("degraded_since = %v, want the FIRST failure %v", s.DegradedSince, firstFail)
	}
	if s.LastError == "" || s.Public().LastError != "" {
		t.Fatalf("last_error must be kept on the full snapshot and dropped from Public(): %q / %q", s.LastError, s.Public().LastError)
	}

	h.RecordSuccess()
	s = h.Snapshot()
	if s.State != EmbedHealthOK || s.ConsecutiveFailures != 0 || s.TotalFailures != 2 || s.DegradedSince != nil {
		t.Fatalf("after recovery: %+v, want ok, consecutive 0, total 2", s)
	}
}

func TestEmbedHealth_CallerCancellationIsNotABackendFailure(t *testing.T) {
	h := NewEmbedHealth("embed-test")
	h.RecordSuccess()
	h.RecordFailure(context.Canceled)
	if got := h.Snapshot().State; got != EmbedHealthOK {
		t.Fatalf("state after a caller cancellation = %q, want ok", got)
	}
	h.RecordFailure(context.DeadlineExceeded)
	if got := h.Snapshot().State; got != EmbedHealthDegraded {
		t.Fatalf("state after a deadline = %q, want degraded (a backend too slow for the recall budget degrades recall)", got)
	}
}

func TestEmbedHealth_DisabledIsStickyAndDegraded(t *testing.T) {
	h := NewEmbedHealth("embed-openai")
	h.MarkDisabled("configured provider failed to initialize at startup")
	h.RecordSuccess() // must not mask a startup failure
	s := h.Snapshot()
	if s.State != EmbedHealthDisabled || !s.Degraded() || s.Reason == "" {
		t.Fatalf("disabled snapshot = %+v, want sticky disabled with a reason", s)
	}
	u := NewEmbedHealth("")
	u.MarkUnconfigured("no embed provider configured")
	if u.Snapshot().Degraded() {
		t.Fatal("unconfigured is a choice, not a degradation")
	}
}

func TestEmbedHealth_NilSafe(t *testing.T) {
	var h *EmbedHealth
	h.RecordSuccess()
	h.RecordFailure(errors.New("x"))
	h.RecordDegradedRecall()
	h.RecordZeroVectorRefused()
	h.MarkDisabled("x")
	if got := h.Snapshot().State; got != EmbedHealthUnknown {
		t.Fatalf("nil tracker state = %q, want unknown", got)
	}
}

// TestRunEmbedHealthProbe_ProbesWhenIdle: with no organic embed traffic the
// probe must still exercise the backend, or a backend that dies while the
// instance is idle reads "ok" forever.
func TestRunEmbedHealthProbe_ProbesWhenIdle(t *testing.T) {
	h := NewEmbedHealth("embed-test")
	var calls atomic.Int32
	embed := func(ctx context.Context, texts []string) ([]float32, error) {
		calls.Add(1)
		err := errors.New("connection refused")
		h.RecordFailure(err) // what the instrumented EmbedService does
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunEmbedHealthProbe(ctx, h, embed, 10*time.Millisecond, time.Second); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() < 2 {
		t.Fatalf("probe ran %d times, want >= 2 (initial + at least one idle tick)", calls.Load())
	}
	if got := h.Snapshot().State; got != EmbedHealthDegraded {
		t.Fatalf("state = %q, want degraded", got)
	}
}

// TestRetroactiveProcessor_RefusesZeroVector pins G4's store-path guard: a
// provider that "succeeds" with an all-zero vector for an engram must not
// have that vector persisted and flagged as embedded (it would never be
// retried and would score 0 against every query). The engram stays pending,
// the refusal is counted, and its batch-mate with a real vector is stored.
func TestRetroactiveProcessor_RefusesZeroVector(t *testing.T) {
	engrams := []*Engram{
		{ID: ULID{1}, Concept: "zero", Content: "zero-me"},
		{ID: ULID{2}, Concept: "real", Content: "fine"},
	}
	store := &mockPluginStore{countResult: 2, scanResult: &mockIterator{engrams: engrams}}
	p := &embedMockForRetro{
		mockPlugin: mockPlugin{name: "embed-retro", tier: TierEmbed},
		embedFunc: func(texts []string) ([]float32, error) {
			out := make([]float32, 0, 3*len(texts))
			for _, tx := range texts {
				if tx == "zero-me" || tx == "zero zero-me" {
					out = append(out, 0, 0, 0)
				} else {
					out = append(out, 0.6, 0.8, 0)
				}
			}
			return out, nil
		},
	}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)
	h := NewEmbedHealth("embed-retro")
	rp.SetEmbedHealth(h)

	if !rp.processBatch(context.Background()) {
		t.Fatal("processBatch returned false")
	}
	if store.updateEmbedCalls != 1 {
		t.Fatalf("UpdateEmbedding calls = %d, want 1 (the zero vector must not be persisted)", store.updateEmbedCalls)
	}
	if store.setFlagCalls != 1 {
		t.Fatalf("SetDigestFlag calls = %d, want 1 (the zero-vector engram must stay pending, unflagged)", store.setFlagCalls)
	}
	if got := h.Snapshot().ZeroVectorsRefused; got != 1 {
		t.Fatalf("ZeroVectorsRefused = %d, want 1", got)
	}
}

// nonZeroVecs returns n*dim components, all non-zero: a stand-in for a real
// provider's output. Fixtures must not use all-zero vectors as "success" —
// the retroactive embedder refuses to store those (G4).
func nonZeroVecs(n, dim int) []float32 {
	out := make([]float32, n*dim)
	for i := range out {
		out[i] = 0.5
	}
	return out
}
