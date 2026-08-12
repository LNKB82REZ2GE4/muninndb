package consolidation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

func TestDreamOnce_DryRun_NoMutations(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dream_dry"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0}
	id := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "test", Content: "some content", Confidence: 0.8, Relevance: 0.6,
		Stability: 20, Embedding: embed,
	})

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{DryRun: true, Force: true, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 vault report, got %d", len(report.Reports))
	}
	if !report.Reports[0].DryRun {
		t.Error("expected DryRun=true in report")
	}

	// Verify engram is untouched.
	eng, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	if eng.State == storage.StateArchived {
		t.Error("engram should not be archived in dry-run mode")
	}
}

func TestDreamOnce_LegalVaultSkipped(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "legal/docs"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0}
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "contract", Content: "confidential agreement", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed,
	})

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: true, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Skipped) != 1 || report.Skipped[0] != "legal/docs" {
		t.Errorf("expected legal/docs in Skipped, got %v", report.Skipped)
	}

	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(report.Reports))
	}
	r := report.Reports[0]
	if r.LegalSkipped == 0 {
		t.Error("expected LegalSkipped > 0")
	}
	if r.MergedEngrams != 0 {
		t.Error("legal vault should have 0 merged engrams")
	}
}

func TestDreamOnce_ScopeFilter(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()

	for _, vault := range []string{"work", "personal"} {
		wsPrefix := store.ResolveVaultPrefix(vault)
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "test", Content: "content", Confidence: 0.5, Relevance: 0.5,
			Stability: 20, Embedding: []float32{1, 0, 0},
		})
	}

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: true, Scope: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 vault report with scope, got %d", len(report.Reports))
	}
	if report.Reports[0].Vault != "work" {
		t.Errorf("expected vault 'work', got %q", report.Reports[0].Vault)
	}
}

func TestDreamOnce_EmptyVault(t *testing.T) {
	store, _, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: true, Scope: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(report.Reports))
	}
	if report.Reports[0].Orient == nil {
		t.Error("expected orient summary even for empty vault")
	}
}

// TestDreamOnce_RunsSchemaPromotionAndTransitive pins phases 3 and 5 into the
// dream pass: a hub engram (>=10 out-edges, relevance >=0.8) must be promoted,
// and a strong A→B→C triangle must gain an inferred A→C edge. (Upstream only
// wired these into the background scheduler, which the server never starts;
// the fork runs them in the nightly offline dream instead. Phase 4 decay
// acceleration is deliberately NOT wired — hardcoded constants.)
func TestDreamOnce_RunsSchemaPromotionAndTransitive(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dream_phases35"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// Phase 5 seed: strong triangle A→B→C, no A→C.
	a := &storage.Engram{Concept: "a", Content: "content a", Confidence: 1.0, Relevance: 0.8, Stability: 30}
	b := &storage.Engram{Concept: "b", Content: "content b", Confidence: 1.0, Relevance: 0.8, Stability: 30}
	c := &storage.Engram{Concept: "c", Content: "content c", Confidence: 1.0, Relevance: 0.8, Stability: 30}
	idA, _ := store.WriteEngram(ctx, wsPrefix, a)
	idB, _ := store.WriteEngram(ctx, wsPrefix, b)
	idC, _ := store.WriteEngram(ctx, wsPrefix, c)
	store.WriteAssociation(ctx, wsPrefix, idA, idB, &storage.Association{
		TargetID: idB, Weight: 0.8, Confidence: 1.0, RelType: storage.RelSupports, CreatedAt: time.Now(),
	})
	store.WriteAssociation(ctx, wsPrefix, idB, idC, &storage.Association{
		TargetID: idC, Weight: 0.9, Confidence: 1.0, RelType: storage.RelSupports, CreatedAt: time.Now(),
	})

	// Phase 3 seed: hub with 10 out-edges and high relevance.
	hub := &storage.Engram{Concept: "hub", Content: "hub content", Confidence: 1.0, Relevance: 0.9, Stability: 30}
	idHub, _ := store.WriteEngram(ctx, wsPrefix, hub)
	for i := 0; i < 10; i++ {
		spoke := &storage.Engram{
			Concept: fmt.Sprintf("spoke-%d", i), Content: fmt.Sprintf("spoke content %d", i),
			Confidence: 1.0, Relevance: 0.5, Stability: 30,
		}
		idS, _ := store.WriteEngram(ctx, wsPrefix, spoke)
		store.WriteAssociation(ctx, wsPrefix, idHub, idS, &storage.Association{
			TargetID: idS, Weight: 0.5, Confidence: 1.0, RelType: storage.RelSupports, CreatedAt: time.Now(),
		})
	}

	w := NewWorker(&mockEngineInterface{store: store})
	report, err := w.DreamOnce(ctx, DreamOpts{Scope: vault})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 vault report, got %d", len(report.Reports))
	}
	r := report.Reports[0]

	if r.InferredEdges < 1 {
		t.Errorf("phase 5 not run in dream: InferredEdges = %d, want >= 1", r.InferredEdges)
	}
	if r.PromotedNodes < 1 {
		t.Errorf("phase 3 not run in dream: PromotedNodes = %d, want >= 1", r.PromotedNodes)
	}

	// Phase 4 must NOT run in dream (deliberately unwired).
	if r.DecayedEngrams != 0 {
		t.Errorf("phase 4 must not run in dream: DecayedEngrams = %d, want 0", r.DecayedEngrams)
	}
}
