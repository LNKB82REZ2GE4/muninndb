package consolidation

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestDedup_DistinctFactPairsSurviveEndToEnd drives the real dedup phase at the
// dream threshold (cosine >= 0.85) over pairs whose embeddings are
// near-identical (cosine ~0.9999) but which state DIFFERENT facts: they differ
// only by a number, an explicit negation, a version string, a name, an
// identifier, or a morphological negation. Every one must survive with neither
// member archived. Genuine restatements must still merge, so the guard cannot
// pass by refusing everything.
//
// Before dedup_identity.go, name swap, identifier swap, morphological negation
// and enable/disable all merged (the loser archived); number, explicit negation
// and version were already kept apart by dedup_separation.go.
func TestDedup_DistinctFactPairsSurviveEndToEnd(t *testing.T) {
	cases := []struct {
		name      string
		a, b      string
		context   []string // other engrams in the vault (orthogonal embeddings)
		wantMerge bool
	}{
		{"number", "The request timeout is 30s for the ingest client",
			"The request timeout is 60s for the ingest client", nil, false},
		{"explicit negation", "Prospective memory delivery is enabled on the server",
			"Prospective memory delivery is not enabled on the server", nil, false},
		{"version string", "The fork is now running v0.10.0 after the merge",
			"The fork is now running v0.11.0 after the merge", nil, false},
		{"name swap mid-sentence", "The deploy pipeline is owned by Priya and she approves releases",
			"The deploy pipeline is owned by Marcus and he approves releases", nil, false},
		// Sentence-initial names are only names when the vault itself writes
		// them capitalized mid-sentence.
		{"name swap sentence-initial", "Priya owns the deploy pipeline and approves every release",
			"Marcus owns the deploy pipeline and approves every release",
			[]string{"Asked Priya about the backlog today", "The review went to Marcus on Friday"}, false},
		{"identifier swap", "The worker reads its queue from QueueStore and retries on failure",
			"The worker reads its queue from JobLedger and retries on failure", nil, false},
		{"morphological negation", "Auto-linking of neighbours is supported for this vault",
			"Auto-linking of neighbours is unsupported for this vault", nil, false},
		{"enable/disable polarity", "Prospective memory delivery is enabled on the server",
			"Prospective memory delivery is disabled on the server", nil, false},

		// Controls: real restatements must still merge.
		{"control: exact repeat", "Priya owns the deploy pipeline", "Priya owns the deploy pipeline", nil, true},
		{"control: casing and punctuation", "Priya owns the deploy pipeline.", "priya owns the deploy pipeline", nil, true},
		{"control: sentence-initial word change", "Use uv for every Python install on this box",
			"Prefer uv for every Python install on this box", nil, true},
		{"control: sentence-initial word change with a vault profile", "Use uv for every Python install on this box",
			"Prefer uv for every Python install on this box",
			[]string{"we use uv everywhere", "I prefer uv over pip"}, true},
		{"control: elaboration adds a name", "The deploy pipeline is owned by Priya",
			"The deploy pipeline is owned by Priya, with Marcus as backup", nil, true},
		{"control: name only lowercased on one side", "Restart Muninn after the upgrade lands",
			"restart muninn after the upgrade lands", nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, db, cleanup := testStoreWithDB(t)
			defer cleanup()
			ctx := context.Background()
			vault := "dedup_identity"
			ws := store.ResolveVaultPrefix(vault)

			idA := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
				Concept: "fact", Content: tc.a, Confidence: 0.9, Relevance: 0.9, Stability: 30,
				Embedding: []float32{1, 0.01, 0, 0, 0, 0},
			})
			idB := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
				Concept: "fact", Content: tc.b, Confidence: 0.5, Relevance: 0.5, Stability: 30,
				Embedding: []float32{1, 0, 0.01, 0, 0, 0},
			})
			for i, c := range tc.context {
				vec := make([]float32, 6)
				vec[3+i] = 1
				writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
					Concept: "context", Content: c, Confidence: 0.5, Relevance: 0.5, Stability: 30, Embedding: vec,
				})
			}

			w := &Worker{Engine: &mockEngineInterface{store: store}, MaxDedup: 100, MaxTransitive: 100, DedupThreshold: 0.85}
			report := &ConsolidationReport{}
			if err := w.runPhase2Dedup(ctx, store, ws, report, vault); err != nil {
				t.Fatal(err)
			}
			if report.DedupClusters != 1 {
				t.Fatalf("DedupClusters = %d, want 1 (fixture must be a dedup candidate)", report.DedupClusters)
			}

			archived := 0
			for _, id := range []storage.ULID{idA, idB} {
				got, err := store.GetEngram(ctx, ws, id)
				if err != nil {
					t.Fatal(err)
				}
				if got.State == storage.StateArchived {
					archived++
				}
			}
			if tc.wantMerge {
				if report.MergedEngrams != 1 || archived != 1 {
					t.Errorf("restatement not merged: merged=%d archived=%d (%q | %q)", report.MergedEngrams, archived, tc.a, tc.b)
				}
				return
			}
			if report.MergedEngrams != 0 || archived != 0 {
				t.Errorf("distinct facts merged: merged=%d archived=%d (%q | %q)", report.MergedEngrams, archived, tc.a, tc.b)
			}
			if report.DedupSeparated != 1 {
				t.Errorf("DedupSeparated = %d, want 1", report.DedupSeparated)
			}
		})
	}
}

func TestPolarityPair(t *testing.T) {
	cases := []struct {
		pos, neg string
		want     bool
	}{
		{"supported", "unsupported", true},
		{"blocking", "nonblocking", true},
		{"connected", "disconnected", true},
		{"correct", "incorrect", true},
		{"possible", "impossible", true},
		{"enabled", "disabled", true},
		{"include", "exclude", true},
		{"import", "export", true},
		// Short bases and look-alikes must not fire.
		{"put", "input", false},
		{"form", "inform", false}, // base >= 4 but "inform" is in+form: accepted cost, see below
		{"able", "table", false},
		{"enabled", "enable", false},
		{"use", "reuse", false},
	}
	for _, tc := range cases {
		got := polarityPair(tc.pos, tc.neg) || polarityPair(tc.neg, tc.pos)
		if tc.pos == "form" {
			// in+form matches the in- rule by construction. Pinned so the cost
			// is visible: such a pair only blocks when "form" and "inform" are
			// each absent from the other memory, i.e. the texts really differ.
			if !got {
				t.Errorf("in+form: rule changed; update the documented cost")
			}
			continue
		}
		if got != tc.want {
			t.Errorf("polarityPair(%q, %q) = %v, want %v", tc.pos, tc.neg, got, tc.want)
		}
	}
}

func TestDivergesOnIdentity_Profile(t *testing.T) {
	prof := newCaseProfile()
	for _, c := range []string{"asked Priya twice", "then Priya agreed", "we use uv", "Use it daily. We use uv"} {
		prof.add(&storage.Engram{Content: c})
	}
	if !prof.isName("priya") {
		t.Error("priya is capitalized in every mid-sentence occurrence; want name")
	}
	if prof.isName("use") {
		t.Error("use is lowercase mid-sentence; want not a name")
	}
	if prof.isName("never-seen") {
		t.Error("a word with no mid-sentence evidence must not be a name")
	}
	var nilProf *caseProfile
	if nilProf.isName("priya") {
		t.Error("nil profile must never classify a name")
	}

	a := &storage.Engram{Content: "Priya owns it"}
	b := &storage.Engram{Content: "Marcus owns it"}
	if divergesOnIdentity(a, b, nil) {
		t.Error("without a profile, sentence-initial capitals are not names")
	}
	prof.add(&storage.Engram{Content: "ask Marcus"})
	if !divergesOnIdentity(a, b, prof) || !divergesOnIdentity(b, a, prof) {
		t.Error("with profile evidence for both names the swap must diverge, symmetrically")
	}
	if divergesOnIdentity(&storage.Engram{Content: "I think it works"}, &storage.Engram{Content: "We think it works"}, prof) {
		t.Error("the pronoun I is never a name")
	}
}
