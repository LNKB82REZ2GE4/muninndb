package consolidation

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestDedup_KnowledgePageIsNeverClustered pins the dedup exemption for
// knowledge pages (engrams tagged exactly "kind:knowledge-page", written by an
// external page-writer that summarizes cited source engrams).
//
// A page that restates its source with the SAME load-bearing tokens (same
// numbers, same identifiers, no negation) defeats both the separation guard and
// the identity guard, so at the dream threshold (cosine >= 0.85) one of the two
// was archived: either the canonical source in favour of an LLM-written summary
// (page wins the Relevance*Confidence election) or the page (source wins). Both
// are wrong. The page must never be a dedup loser and never a survivor that
// absorbs a source, so it is excluded from candidate pairs entirely.
//
// Without the exemption every case below archives one engram.
func TestDedup_KnowledgePageIsNeverClustered(t *testing.T) {
	const source = "The ingest worker retries 3 times with a 30s timeout before it parks a job"
	const page = "The ingest worker retries 3 times with a 30s timeout before it parks a job."

	cases := []struct {
		name                string
		pageScore, srcScore float32
	}{
		{"page outranks its source", 0.9, 0.5},
		{"source outranks its page", 0.5, 0.9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, db, cleanup := testStoreWithDB(t)
			defer cleanup()
			ctx := context.Background()
			vault := "dedup_pages"
			ws := store.ResolveVaultPrefix(vault)

			pageTags := []string{KnowledgePageTag, "page:ingest-retries"}
			idPage := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
				Concept: "knowledge page: ingest retries", Content: page, Tags: pageTags,
				Confidence: tc.pageScore, Relevance: tc.pageScore, Stability: 30,
				Embedding: []float32{1, 0.01, 0, 0},
			})
			idSrc := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
				Concept: "ingest retries", Content: source, Tags: []string{"topic:ingest"},
				Confidence: tc.srcScore, Relevance: tc.srcScore, Stability: 30,
				Embedding: []float32{1, 0, 0.01, 0},
			})
			if sim := cosineSimilarity([]float32{1, 0.01, 0, 0}, []float32{1, 0, 0.01, 0}); sim < 0.85 {
				t.Fatalf("fixture cosine %.4f < 0.85: not a dedup candidate", sim)
			}

			w := &Worker{Engine: &mockEngineInterface{store: store}, MaxDedup: 100, MaxTransitive: 100, DedupThreshold: 0.85}
			report := &ConsolidationReport{}
			if err := w.runPhase2Dedup(ctx, store, ws, report, vault); err != nil {
				t.Fatal(err)
			}

			for _, id := range []storage.ULID{idPage, idSrc} {
				got, err := store.GetEngram(ctx, ws, id)
				if err != nil {
					t.Fatal(err)
				}
				if got.State == storage.StateArchived {
					t.Errorf("engram %s (%q) archived by dedup; a knowledge page and its source must both survive", id, got.Concept)
				}
			}
			if report.MergedEngrams != 0 || report.DedupClusters != 0 {
				t.Errorf("merged=%d clusters=%d, want 0/0: a page must not enter a candidate pair", report.MergedEngrams, report.DedupClusters)
			}
			gotSrc, err := store.GetEngram(ctx, ws, idSrc)
			if err != nil {
				t.Fatal(err)
			}
			for _, tag := range gotSrc.Tags {
				if tag == KnowledgePageTag {
					t.Errorf("source absorbed the page tag %q: tags folded across a page/source pair", tag)
				}
			}
		})
	}
}

// TestDedup_KnowledgePageDoesNotShieldDuplicateSources is the control: the
// exemption removes only the page. Two true-duplicate sources sitting next to a
// page still merge, and the page neither absorbs the loser nor gets archived.
func TestDedup_KnowledgePageDoesNotShieldDuplicateSources(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()
	ctx := context.Background()
	vault := "dedup_pages_control"
	ws := store.ResolveVaultPrefix(vault)

	const text = "The ingest worker retries 3 times with a 30s timeout before it parks a job"
	idPage := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
		Concept: "knowledge page: ingest retries", Content: text + ".", Tags: []string{KnowledgePageTag},
		Confidence: 0.99, Relevance: 0.99, Stability: 30, Embedding: []float32{1, 0.01, 0, 0},
	})
	idA := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
		Concept: "ingest retries", Content: text, Tags: []string{"topic:ingest"},
		Confidence: 0.9, Relevance: 0.9, Stability: 30, Embedding: []float32{1, 0, 0.01, 0},
	})
	idB := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
		Concept: "ingest retries", Content: text, Tags: []string{"topic:ingest"},
		Confidence: 0.5, Relevance: 0.5, Stability: 30, Embedding: []float32{1, 0, 0, 0.01},
	})

	w := &Worker{Engine: &mockEngineInterface{store: store}, MaxDedup: 100, MaxTransitive: 100, DedupThreshold: 0.85}
	report := &ConsolidationReport{}
	if err := w.runPhase2Dedup(ctx, store, ws, report, vault); err != nil {
		t.Fatal(err)
	}
	if report.DedupClusters != 1 || report.MergedEngrams != 1 {
		t.Fatalf("clusters=%d merged=%d, want 1/1 (the duplicate sources still merge)", report.DedupClusters, report.MergedEngrams)
	}
	state := func(id storage.ULID) *storage.Engram {
		got, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if p := state(idPage); p.State == storage.StateArchived || len(p.Tags) != 1 || p.AccessCount != 0 {
		t.Errorf("page touched by dedup: state=%v tags=%v access=%d", p.State, p.Tags, p.AccessCount)
	}
	if state(idA).State == storage.StateArchived {
		t.Error("higher-scored source archived; it should be the survivor")
	}
	if state(idB).State != storage.StateArchived {
		t.Error("lower-scored duplicate source not archived")
	}
}
