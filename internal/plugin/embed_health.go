package plugin

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Embed backend health states reported by EmbedHealth.Snapshot.
//
// The recall pipeline degrades to BM25-only when the embedder fails (#578):
// that is the intended, graceful half of CLAUDE.md principle 2. EmbedHealth
// is the loud half — one process-wide record of whether the semantic signal
// is actually available, so an operator, a hook, or a poller can see the
// degradation without scraping the per-call WARN line.
const (
	// EmbedHealthUnknown: an embedder is configured but no embed call has
	// completed yet (the startup probe has not landed).
	EmbedHealthUnknown = "unknown"
	// EmbedHealthOK: the most recent embed call returned a usable vector.
	EmbedHealthOK = "ok"
	// EmbedHealthDegraded: the most recent embed call failed or returned an
	// empty/all-zero vector. Recall is BM25-only until a call succeeds.
	EmbedHealthDegraded = "degraded"
	// EmbedHealthDisabled: an embed provider was requested but could not be
	// initialized at startup, so the process runs with no embedder at all.
	// This does not self-heal: it needs the provider fixed and a restart.
	EmbedHealthDisabled = "disabled"
	// EmbedHealthUnconfigured: no embed provider was requested. Semantic
	// recall is off by choice; nothing to alert on.
	EmbedHealthUnconfigured = "unconfigured"
)

// EmbedHealthSnapshot is the wire shape of the embed backend's health. It is
// served by muninn_status (MCP), GET /api/health and GET
// /api/admin/embed/status (REST).
type EmbedHealthSnapshot struct {
	State    string `json:"state"`
	Provider string `json:"provider,omitempty"`
	// Reason explains a disabled/unconfigured state.
	Reason string `json:"reason,omitempty"`
	// DegradedSince is the time of the first failure in the current run of
	// consecutive failures; nil when the backend is not degraded.
	DegradedSince       *time.Time `json:"degraded_since,omitempty"`
	ConsecutiveFailures int64      `json:"consecutive_failures"`
	TotalFailures       int64      `json:"total_failures"`
	LastOK              *time.Time `json:"last_ok,omitempty"`
	LastFailure         *time.Time `json:"last_failure,omitempty"`
	// LastError is the most recent failure's message. Omitted on the public,
	// unauthenticated health endpoint (see Public).
	LastError string `json:"last_error,omitempty"`
	// DegradedRecalls counts per-vault activations served without a semantic
	// signal (BM25-only) since startup. A multi-vault recall counts once per
	// vault it searched.
	DegradedRecalls int64 `json:"degraded_recalls"`
	// ZeroVectorsRefused counts embeddings the background embedder refused to
	// persist because the provider returned an all-zero vector for them. Those
	// engrams stay pending and are retried.
	ZeroVectorsRefused int64 `json:"zero_vectors_refused"`
}

// Public returns a copy safe for an unauthenticated endpoint: the provider
// error text (which can carry endpoint URLs) is dropped.
func (s EmbedHealthSnapshot) Public() EmbedHealthSnapshot {
	s.LastError = ""
	return s
}

// Degraded reports whether the snapshot describes a state in which recall
// is silently missing its semantic signal.
func (s EmbedHealthSnapshot) Degraded() bool {
	return s.State == EmbedHealthDegraded || s.State == EmbedHealthDisabled
}

// EmbedHealth tracks the embed backend's health from the outcome of every
// embed call made through the instrumented EmbedService (recall-time query
// embedding, background retroactive embedding, and the idle probe). All
// methods are safe for concurrent use and are no-ops on a nil receiver, so
// call sites need no wiring checks.
type EmbedHealth struct {
	mu sync.Mutex

	provider string
	// fixedState is set for disabled/unconfigured: those states are decided
	// once at startup and are not changed by call outcomes.
	fixedState string
	reason     string

	lastOK        time.Time
	lastFailure   time.Time
	degradedSince time.Time
	lastActivity  time.Time
	consecutive   int64
	totalFailures int64
	lastErr       string

	degradedRecalls    int64
	zeroVectorsRefused int64

	now func() time.Time
}

// NewEmbedHealth returns a tracker for a configured, initialized provider.
// Its state is "unknown" until the first embed call completes.
func NewEmbedHealth(provider string) *EmbedHealth {
	return &EmbedHealth{provider: provider, now: time.Now}
}

// MarkDisabled records that a requested provider failed to initialize, so
// the process has no embedder. Logged at ERROR: this state never self-heals.
func (h *EmbedHealth) MarkDisabled(reason string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.fixedState = EmbedHealthDisabled
	h.reason = reason
	h.mu.Unlock()
}

// MarkUnconfigured records that no embed provider was requested.
func (h *EmbedHealth) MarkUnconfigured(reason string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.fixedState = EmbedHealthUnconfigured
	h.reason = reason
	h.mu.Unlock()
}

// RecordSuccess records an embed call that returned a usable vector.
func (h *EmbedHealth) RecordSuccess() {
	if h == nil {
		return
	}
	h.mu.Lock()
	now := h.now()
	wasDegraded := h.consecutive > 0
	since := h.degradedSince
	failures := h.consecutive
	h.lastOK = now
	h.lastActivity = now
	h.consecutive = 0
	h.degradedSince = time.Time{}
	h.mu.Unlock()
	if wasDegraded {
		slog.Info("embed backend recovered: semantic recall restored",
			"provider", h.provider, "degraded_for", now.Sub(since).Round(time.Second),
			"failed_calls", failures)
	}
}

// RecordFailure records an embed call that failed or returned an unusable
// vector. A caller-side cancellation (context.Canceled) says nothing about
// the backend and is ignored; a deadline is counted, because a backend too
// slow to answer inside the recall budget is degrading recall.
func (h *EmbedHealth) RecordFailure(err error) {
	if h == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	h.mu.Lock()
	now := h.now()
	first := h.consecutive == 0
	h.consecutive++
	h.totalFailures++
	h.lastFailure = now
	h.lastActivity = now
	h.lastErr = truncateErr(err.Error())
	if first {
		h.degradedSince = now
	}
	h.mu.Unlock()
	if first {
		// ERROR, once per transition (not per call): this is the line an
		// operator greps for. Each degraded recall still logs its own WARN.
		slog.Error("embed backend DEGRADED: semantic recall unavailable, serving BM25-only results until it recovers",
			"provider", h.provider, "error", err)
	}
}

// RecordDegradedRecall counts one activation served without a semantic signal.
func (h *EmbedHealth) RecordDegradedRecall() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.degradedRecalls++
	h.mu.Unlock()
}

// RecordZeroVectorRefused counts one embedding refused at store time.
func (h *EmbedHealth) RecordZeroVectorRefused() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.zeroVectorsRefused++
	h.mu.Unlock()
}

// IdleFor returns how long since the last recorded embed outcome; a tracker
// that has never recorded one reports a very large duration.
func (h *EmbedHealth) IdleFor() time.Duration {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastActivity.IsZero() {
		return time.Duration(1<<63 - 1)
	}
	return h.now().Sub(h.lastActivity)
}

// Snapshot returns the current health.
func (h *EmbedHealth) Snapshot() EmbedHealthSnapshot {
	if h == nil {
		return EmbedHealthSnapshot{State: EmbedHealthUnknown}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := EmbedHealthSnapshot{
		Provider:            h.provider,
		Reason:              h.reason,
		ConsecutiveFailures: h.consecutive,
		TotalFailures:       h.totalFailures,
		LastError:           h.lastErr,
		DegradedRecalls:     h.degradedRecalls,
		ZeroVectorsRefused:  h.zeroVectorsRefused,
	}
	if !h.lastOK.IsZero() {
		t := h.lastOK
		s.LastOK = &t
	}
	if !h.lastFailure.IsZero() {
		t := h.lastFailure
		s.LastFailure = &t
	}
	switch {
	case h.fixedState != "":
		s.State = h.fixedState
	case h.consecutive > 0:
		s.State = EmbedHealthDegraded
		t := h.degradedSince
		s.DegradedSince = &t
	case !h.lastOK.IsZero():
		s.State = EmbedHealthOK
	default:
		s.State = EmbedHealthUnknown
	}
	return s
}

// IsZeroVector reports whether vec is empty or every component is exactly
// zero. A normalized embedder never emits that for real text.
func IsZeroVector(vec []float32) bool {
	for _, v := range vec {
		if v != 0 {
			return false
		}
	}
	return true
}

// ErrZeroVector is recorded when a provider reports success but returns an
// empty or all-zero embedding for non-blank input.
var ErrZeroVector = errors.New("embed provider returned an empty or all-zero vector")

// AllBlank reports whether every text is empty after trimming whitespace.
func AllBlank(texts []string) bool {
	for _, t := range texts {
		if strings.TrimSpace(t) != "" {
			return false
		}
	}
	return true
}

func truncateErr(s string) string {
	const max = 300
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// RunEmbedHealthProbe keeps h fresh while the instance is idle: every
// interval, if no embed outcome was recorded during the last interval, it
// issues one tiny embed call through embed (which records its own outcome
// when embed is the instrumented EmbedService). Without it a backend that
// dies while nobody is recalling or writing would read "ok" indefinitely.
// It returns when ctx is cancelled.
func RunEmbedHealthProbe(ctx context.Context, h *EmbedHealth, embed func(context.Context, []string) ([]float32, error), interval, timeout time.Duration) {
	if h == nil || embed == nil || interval <= 0 {
		return
	}
	probe := func() {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		// The instrumented EmbedService records the outcome itself; the
		// probe only needs to make the call.
		_, _ = embed(pctx, []string{"embed health probe"})
	}
	if h.IdleFor() >= interval {
		probe()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if h.IdleFor() >= interval {
				probe()
			}
		}
	}
}
