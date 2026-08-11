package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ── mergeRRF unit tests (pure function, no engine needed) ──────────────────

func TestMergeRRF_WeightOrdering(t *testing.T) {
	// Two vaults, equal-length result sets, unequal weights. The
	// higher-weighted vault's rank-1 item must outscore the lower-weighted
	// vault's rank-1 item, and ordering must follow weight/(k+rank).
	runs := []vaultRun{
		{vault: "a", resp: &mbp.ActivateResponse{Activations: []mbp.ActivationItem{
			{ID: "a1"}, {ID: "a2"},
		}}},
		{vault: "b", resp: &mbp.ActivateResponse{Activations: []mbp.ActivationItem{
			{ID: "b1"}, {ID: "b2"},
		}}},
	}
	merged := mergeRRF(runs, []float64{0.8, 0.2}, 10, 10)
	if len(merged) != 4 {
		t.Fatalf("len(merged) = %d, want 4", len(merged))
	}
	if merged[0].ID != "a1" {
		t.Errorf("top result = %q, want a1 (highest weight, rank 1)", merged[0].ID)
	}
	// a1 score = 0.8/(10+1) = 0.0727; b1 score = 0.2/(10+1) = 0.0182;
	// a2 score = 0.8/(10+2) = 0.0667. So order should be a1, a2, b1, b2.
	want := []string{"a1", "a2", "b1", "b2"}
	for i, id := range want {
		if merged[i].ID != id {
			t.Errorf("merged[%d].ID = %q, want %q (full order: %v)", i, merged[i].ID, id, idsOfItems(merged))
		}
	}
}

func TestMergeRRF_FloorGuaranteesSlotForQuietVault(t *testing.T) {
	// Vault "big" has many high-rank items; vault "quiet" has exactly one,
	// weighted so low its raw RRF score would never make the cut. The floor
	// must still reserve it a slot.
	bigActs := make([]mbp.ActivationItem, 20)
	for i := range bigActs {
		bigActs[i] = mbp.ActivationItem{ID: "big" + string(rune('a'+i))}
	}
	runs := []vaultRun{
		{vault: "big", resp: &mbp.ActivateResponse{Activations: bigActs}},
		{vault: "quiet", resp: &mbp.ActivateResponse{Activations: []mbp.ActivationItem{{ID: "q1"}}}},
	}
	merged := mergeRRF(runs, []float64{0.99, 0.01}, 5, 10)
	if len(merged) != 5 {
		t.Fatalf("len(merged) = %d, want 5 (limit)", len(merged))
	}
	found := false
	for _, item := range merged {
		if item.ID == "q1" {
			found = true
		}
	}
	if !found {
		t.Errorf("quiet vault's only result (q1) was crowded out entirely: %v", idsOfItems(merged))
	}
}

func TestMergeRRF_LimitCut(t *testing.T) {
	runs := []vaultRun{
		{vault: "a", resp: &mbp.ActivateResponse{Activations: []mbp.ActivationItem{
			{ID: "a1"}, {ID: "a2"}, {ID: "a3"},
		}}},
	}
	merged := mergeRRF(runs, []float64{1.0}, 2, 10)
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2 (limit)", len(merged))
	}
	if merged[0].ID != "a1" || merged[1].ID != "a2" {
		t.Errorf("merged = %v, want [a1 a2]", idsOfItems(merged))
	}
}

func TestMergeRRF_EqualWeightsDefault(t *testing.T) {
	runs := []vaultRun{
		{vault: "a", resp: &mbp.ActivateResponse{Activations: []mbp.ActivationItem{{ID: "a1"}}}},
		{vault: "b", resp: &mbp.ActivateResponse{Activations: []mbp.ActivationItem{{ID: "b1"}}}},
	}
	merged := mergeRRF(runs, []float64{0.5, 0.5}, 10, 10)
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
	// Both rank-1 with equal weight: scores tie, stable sort preserves
	// input order (vault "a" processed first).
	if merged[0].ID != "a1" || merged[1].ID != "b1" {
		t.Errorf("merged = %v, want [a1 b1] under tie-break stability", idsOfItems(merged))
	}
}

func idsOfItems(items []mbp.ActivationItem) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

// ── ActivateMulti integration tests (real engine, real storage) ────────────

func TestActivateMulti_MergesAcrossVaults(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	mustWrite(t, eng, "proj-a", "Go concurrency patterns", "Goroutines and channels make concurrent Go code readable.", []string{"golang"})
	mustWrite(t, eng, "agent-memory", "General Go note", "Go was designed at Google for simplicity and concurrency.", []string{"golang"})
	awaitFTS(t, eng)

	reqs := []*mbp.ActivateRequest{
		{Vault: "proj-a", Context: []string{"Go concurrency"}, MaxResults: 10, Threshold: 0.01},
		{Vault: "agent-memory", Context: []string{"Go concurrency"}, MaxResults: 10, Threshold: 0.01},
	}
	resp, err := eng.ActivateMulti(ctx, reqs, []float64{0.67, 0.33})
	if err != nil {
		t.Fatalf("ActivateMulti: %v", err)
	}
	if len(resp.Activations) == 0 {
		t.Fatal("ActivateMulti returned 0 results, want >= 1")
	}
	seenVaults := map[string]bool{}
	for _, item := range resp.Activations {
		if item.Vault == "" {
			t.Errorf("activation item %q missing Vault field in merged response", item.ID)
		}
		seenVaults[item.Vault] = true
	}
	if len(seenVaults) != 2 {
		t.Errorf("expected results tagged from both vaults, got vaults=%v", seenVaults)
	}
}

func TestActivateMulti_RequiresMatchingWeightsLength(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	reqs := []*mbp.ActivateRequest{
		{Vault: "a", Context: []string{"x"}},
		{Vault: "b", Context: []string{"x"}},
	}
	if _, err := eng.ActivateMulti(ctx, reqs, []float64{1.0}); err == nil {
		t.Fatal("expected error for mismatched weights/vaults length, got nil")
	}
}

func TestActivateMulti_RequiresAtLeastOneVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.ActivateMulti(ctx, nil, nil); err == nil {
		t.Fatal("expected error for zero vaults, got nil")
	}
}

// TestActivate_SingleVaultWireCompat verifies that Engine.Activate's own
// response items never populate Vault — that field is additive and set only
// by ActivateMulti (project-vaults phase 2), so ordinary single-vault callers
// see byte-identical responses to before this feature existed.
func TestActivate_SingleVaultWireCompat(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	mustWrite(t, eng, "wire-compat", "Wire compat concept", "Content used to check the Vault field stays empty.", nil)
	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault: "wire-compat", Context: []string{"wire compat"}, MaxResults: 10, Threshold: 0.01,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.DegradedVaults) != 0 {
		t.Errorf("single-vault Activate response must never set DegradedVaults, got %v", resp.DegradedVaults)
	}
	for _, item := range resp.Activations {
		if item.Vault != "" {
			t.Errorf("single-vault Activate response item %q has Vault=%q, want empty", item.ID, item.Vault)
		}
	}
}

func mustWrite(t *testing.T, eng *Engine, vault, concept, content string, tags []string) string {
	t.Helper()
	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: vault, Concept: concept, Content: content, Tags: tags,
	})
	if err != nil {
		t.Fatalf("Write(%q): %v", concept, err)
	}
	return resp.ID
}
