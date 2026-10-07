package engine

import (
	"context"
	"testing"

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
