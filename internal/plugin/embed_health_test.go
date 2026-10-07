package plugin

import (
	"context"
	"errors"
	"strings"
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

// zeroForContentEmbedder returns an all-zero vector for any text containing
// "zero-me" and a real vector otherwise (including the probe text). With
// allZero set it returns zeros for everything: a provider that embeds nothing.
type zeroForContentEmbedder struct {
	embedMockForRetro
	allZero bool
	calls   int
}

func (m *zeroForContentEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	m.calls++
	out := make([]float32, 0, 3*len(texts))
	for _, tx := range texts {
		if m.allZero || strings.Contains(tx, "zero-me") {
			out = append(out, 0, 0, 0)
		} else {
			out = append(out, 0.6, 0.8, 0)
		}
	}
	return out, nil
}

func zeroTestEngrams(nZero int) (zeros []*Engram, good *Engram) {
	for i := 0; i < nZero; i++ {
		id := ULID{}
		id[0], id[1] = byte(i>>8), byte(i)
		zeros = append(zeros, &Engram{ID: id, Concept: "z", Content: "zero-me"})
	}
	good = &Engram{ID: ULID{0xff}, Concept: "real", Content: "fine"}
	return zeros, good
}

// TestRetroactiveProcessor_RefusesZeroVector pins G4's store-path guard: an
// all-zero vector is never persisted as a real embedding. When the provider
// embeds other text fine, the zero is about the engram's content, so it is
// latched like any content-specific failure; batch-mates still embed.
func TestRetroactiveProcessor_RefusesZeroVector(t *testing.T) {
	zeros, good := zeroTestEngrams(1)
	store := newFlagAwareStore(append(zeros, good)...)
	p := &zeroForContentEmbedder{embedMockForRetro: embedMockForRetro{mockPlugin: mockPlugin{name: "embed-retro", tier: TierEmbed}}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)
	h := NewEmbedHealth("embed-retro")
	rp.SetEmbedHealth(h)

	if !rp.processBatch(context.Background()) {
		t.Fatal("processBatch returned false")
	}
	if store.updateEmbedCalls != 1 {
		t.Fatalf("UpdateEmbedding calls = %d, want 1 (the zero vector must not be persisted)", store.updateEmbedCalls)
	}
	if f := store.flags[zeros[0].ID]; f&DigestEmbed != 0 || f&DigestEmbedFailed == 0 {
		t.Fatalf("zero-vector engram flags = %#x, want DigestEmbedFailed and not DigestEmbed", f)
	}
	if f := store.flags[good.ID]; f&DigestEmbed == 0 {
		t.Fatalf("batch-mate flags = %#x, want DigestEmbed", f)
	}
	if got := h.Snapshot().ZeroVectorsRefused; got != 1 {
		t.Fatalf("ZeroVectorsRefused = %d, want 1", got)
	}
}

// TestRetroactiveProcessor_ZeroContentBlockDoesNotLoopOrStarve: a cap-sized
// block of engrams whose content embeds to zero, sorted ahead of a normal
// engram, must drain in bounded passes and must not starve the follower
// (adversarial-review finding: refused-and-pending engrams at the head plus
// the cap's self-Notify made a hot loop).
func TestRetroactiveProcessor_ZeroContentBlockDoesNotLoopOrStarve(t *testing.T) {
	zeros, good := zeroTestEngrams(maxBatchSize + 50)
	store := newFlagAwareStore(append(zeros, good)...)
	p := &zeroForContentEmbedder{embedMockForRetro: embedMockForRetro{mockPlugin: mockPlugin{name: "embed-retro", tier: TierEmbed}}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)
	ctx := context.Background()
	for pass := 0; pass < 3 && store.flags[good.ID]&DigestEmbed == 0; pass++ {
		rp.processBatch(ctx)
	}
	if store.flags[good.ID]&DigestEmbed == 0 {
		t.Fatal("engram behind a block of zero-vector engrams was never embedded (starved)")
	}
	if n := len(store.pending(DigestEmbed, DigestEmbedFailed)); n != 0 {
		t.Fatalf("%d engrams still pending after the block drained, want 0", n)
	}
}

// TestRetroactiveProcessor_AllZeroProviderStopsPass: a provider that returns
// zeros for everything (probe included) is treated as down: nothing is
// stored or latched, the pass stops after one micro-batch, and the processor
// does not self-notify into a hot loop.
func TestRetroactiveProcessor_AllZeroProviderStopsPass(t *testing.T) {
	zeros, good := zeroTestEngrams(maxBatchSize + 50)
	store := newFlagAwareStore(append(zeros, good)...)
	p := &zeroForContentEmbedder{allZero: true, embedMockForRetro: embedMockForRetro{mockPlugin: mockPlugin{name: "embed-retro", tier: TierEmbed}}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)
	h := NewEmbedHealth("embed-retro")
	rp.SetEmbedHealth(h)

	rp.processBatch(context.Background())
	if p.calls > 2 {
		t.Fatalf("provider called %d times in one pass, want <= 2 (one batch + one probe)", p.calls)
	}
	select {
	case <-rp.notifyCh:
		t.Fatal("processor self-notified after finding the provider down (hot loop)")
	default:
	}
	if len(store.flags) != 0 {
		t.Fatalf("flags written while the provider embeds nothing: %d engrams, want 0", len(store.flags))
	}
	if h.Snapshot().ZeroVectorsRefused == 0 {
		t.Fatal("refusals not counted")
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
