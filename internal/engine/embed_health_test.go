package engine

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestActivateMulti_PropagatesSemanticDegraded pins G4: when the embedder
// cannot produce a usable query vector, every per-vault run of a multi-vault
// recall is BM25-only, and the merged response must say so. Before the fix
// ActivateMulti built a fresh response and dropped the flag, so the
// multi-vault path (the one project-scoped clients use) reported BM25-only
// results as if they were semantically ranked.
func TestActivateMulti_PropagatesSemanticDegraded(t *testing.T) {
	eng, cleanup := testEnvWithHNSW(t, zeroVecEmbedder{})
	defer cleanup()
	ctx := context.Background()

	mustWrite(t, eng, "proj-kestrel", "Pump calibration", "The coolant pump was calibrated to 42 litres per minute.", nil)
	mustWrite(t, eng, "general-notes", "Valve torque", "Intake valve bolts are torqued to 18 newton metres.", nil)
	awaitFTS(t, eng)

	reqs := []*mbp.ActivateRequest{
		{Vault: "proj-kestrel", Context: []string{"coolant pump"}, MaxResults: 10, Threshold: 0.01},
		{Vault: "general-notes", Context: []string{"coolant pump"}, MaxResults: 10, Threshold: 0.01},
	}
	resp, err := eng.ActivateMulti(ctx, reqs, []float64{0.67, 0.33})
	if err != nil {
		t.Fatalf("ActivateMulti: %v", err)
	}
	if !resp.SemanticDegraded {
		t.Fatal("ActivateMulti response SemanticDegraded=false although every per-vault run was BM25-only (zero-vector embedder)")
	}
}

// TestActivate_SemanticDegraded_RecordedOnEmbedHealth pins that a degraded
// recall is counted on the attached embed-health tracker, so the health
// surfaces show recalls actually served without semantics.
func TestActivate_SemanticDegraded_RecordedOnEmbedHealth(t *testing.T) {
	eng, cleanup := testEnvWithHNSW(t, zeroVecEmbedder{})
	defer cleanup()
	ctx := context.Background()
	h := plugin.NewEmbedHealth("test")
	eng.SetEmbedHealth(h)

	mustWrite(t, eng, "proj-heron", "Pump calibration", "The coolant pump was calibrated to 42 litres per minute.", nil)
	awaitFTS(t, eng)
	if _, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: "proj-heron", Context: []string{"coolant pump"}}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := eng.EmbedHealth().DegradedRecalls; got != 1 {
		t.Fatalf("EmbedHealth().DegradedRecalls = %d, want 1", got)
	}
}

// TestEngine_EmbedHealth_NoTrackerReportsUnknown: an engine with no tracker
// must not claim the backend is healthy.
func TestEngine_EmbedHealth_NoTrackerReportsUnknown(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	if got := eng.EmbedHealth().State; got != plugin.EmbedHealthUnknown {
		t.Fatalf("EmbedHealth().State = %q without a tracker, want %q", got, plugin.EmbedHealthUnknown)
	}
}

// switchEmbedder returns an all-zero vector (the degraded class) while broken
// is set, and a usable unit vector otherwise.
type switchEmbedder struct{ broken atomic.Bool }

func (s *switchEmbedder) Embed(_ context.Context, _ []string) ([]float32, error) {
	v := make([]float32, 8)
	if !s.broken.Load() {
		v[0] = 1
	}
	return v, nil
}
func (s *switchEmbedder) Tokenize(text string) []string { return []string{text} }

// TestEmbedHealth_FollowsRecallOutcomes pins that EmbedHealth reports what the
// most recent embedding-backed recall observed — the status surface's only
// cheap evidence — and never claims "up" before any recall exercised it.
func TestEmbedHealth_FollowsRecallOutcomes(t *testing.T) {
	emb := &switchEmbedder{}
	eng, cleanup := testEnvWithHNSW(t, emb)
	defer cleanup()
	ctx := context.Background()
	const vault = "embed-health-vault"
	if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "the beehive frames were inspected for queen cells"}); err != nil {
		t.Fatal(err)
	}
	recall := func() {
		t.Helper()
		if _, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"beehive frames"}}); err != nil {
			t.Fatal(err)
		}
	}

	if got := eng.RecallEmbedHealth().State; got != "unknown" {
		t.Fatalf("before any recall: state = %q, want unknown", got)
	}
	emb.broken.Store(true)
	recall()
	if got := eng.RecallEmbedHealth().State; got != "degraded_to_keyword" {
		t.Fatalf("after a degraded recall: state = %q, want degraded_to_keyword", got)
	}
	emb.broken.Store(false)
	recall()
	if got := eng.RecallEmbedHealth().State; got != "up" {
		t.Fatalf("after a recovered recall: state = %q, want up", got)
	}
}

func TestEmbedHealth_NoopIsDisabled(t *testing.T) {
	eng, cleanup := testEnvWithHNSW(t, activation.NewNoopEmbedder())
	defer cleanup()
	if got := eng.RecallEmbedHealth().State; got != "disabled" {
		t.Fatalf("noop embedder: state = %q, want disabled", got)
	}
}
