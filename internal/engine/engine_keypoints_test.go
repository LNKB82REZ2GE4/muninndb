package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// G2: enricher key points are indexed and recall presents the matched one.

const kpTargetPoint = "The heron backfill finished in 41 minutes after the index rebuild."

// kpLongBody is a long session-turn body that never states the key point in the
// key point's words — the shape of a raw turn the enricher distilled.
func kpLongBody() string {
	var b strings.Builder
	b.WriteString("We went back and forth on the plan for most of the afternoon. ")
	for range 30 {
		b.WriteString("Several unrelated tangents about tooling, naming and the weekly sync came up along the way. ")
	}
	b.WriteString("In the end the job completed and everyone signed off.")
	return b.String()
}

// seedKPVault writes a target and some distractors, enriches the target with
// key points through ApplyEnrichment, and returns the target id.
func seedKPVault(t *testing.T, eng *Engine, vault string) storage.ULID {
	t.Helper()
	ctx := context.Background()
	for i, c := range []string{
		"the backfill of the orders table is scheduled for the weekend",
		"index rebuild steps for the search cluster are in the runbook",
		"minutes of the planning meeting were posted to the wiki",
		"the heron dashboard shows queue depth per worker",
		"a short ordinary note about lunch",
		"another short note about the weekly sync",
	} {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "note " + string(rune('a'+i)), Content: c}); err != nil {
			t.Fatalf("Write distractor: %v", err)
		}
	}
	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "session turn", Content: kpLongBody()})
	if err != nil {
		t.Fatalf("Write target: %v", err)
	}
	awaitFTS(t, eng)
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := eng.GetEngram(ctx, vault, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ApplyEnrichment(ctx, vault, &EnrichmentApplyRequest{
		ID:                resp.ID,
		ExpectedUpdatedAt: before.UpdatedAt,
		Summary:           "A long planning discussion that ended with the job completing.",
		KeyPoints: []string{
			"The team discussed tooling and naming conventions.",
			kpTargetPoint,
			"Everyone signed off on the result.",
		},
		StagesCompleted: []string{"summary"},
		Source:          "test-agent",
	}); err != nil {
		t.Fatalf("ApplyEnrichment: %v", err)
	}
	return id
}

func activateKP(t *testing.T, eng *Engine, vault, q string) []mbp.ActivationItem {
	t.Helper()
	resp, err := eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault: vault, Context: []string{q}, MaxResults: 10, Threshold: 0.0,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return resp.Activations
}

func kpRankOf(items []mbp.ActivationItem, id storage.ULID) int {
	for i, it := range items {
		if it.ID == id.String() {
			return i + 1
		}
	}
	return 0
}

// The acceptance line of register item G2: a recall query that matches ONLY
// one key point returns that engram in the top 3, with that key point as the
// matched snippet.
func TestApplyEnrichment_KeyPointMatchRecallsEngramWithSnippet(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "kp-recall"
	id := seedKPVault(t, eng, vault)

	items := activateKP(t, eng, vault, "heron backfill finished minutes after index rebuild")
	r := kpRankOf(items, id)
	if r == 0 || r > 3 {
		t.Fatalf("enriched engram rank = %d, want 1..3 (results: %d)", r, len(items))
	}
	if got := items[r-1].MatchedKeyPoint; got != kpTargetPoint {
		t.Fatalf("MatchedKeyPoint = %q, want %q", got, kpTargetPoint)
	}
	for i, it := range items {
		if i != r-1 && it.MatchedKeyPoint != "" {
			t.Fatalf("row %d (no key points) has MatchedKeyPoint %q", i+1, it.MatchedKeyPoint)
		}
	}
}

// A request carrying ONLY key points was accepted and its key points silently
// dropped: the digest write was gated on summary/type only.
func TestApplyEnrichment_KeyPointsOnlyArePersisted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "kp-only"
	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "c", Content: "some content"})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := storage.ParseULID(resp.ID)
	before, err := eng.GetEngram(ctx, vault, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ApplyEnrichment(ctx, vault, &EnrichmentApplyRequest{
		ID: resp.ID, ExpectedUpdatedAt: before.UpdatedAt,
		KeyPoints: []string{"The only output is this key point."},
	}); err != nil {
		t.Fatalf("ApplyEnrichment(key points only): %v", err)
	}
	after, err := eng.GetEngram(ctx, vault, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.KeyPoints) != 1 || after.KeyPoints[0] != "The only output is this key point." {
		t.Fatalf("KeyPoints = %q, want the one applied", after.KeyPoints)
	}
}

// muninn_read's engine response carries the key points.
func TestRead_ReturnsKeyPoints(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "kp-read"
	id := seedKPVault(t, eng, vault)
	resp, err := eng.Read(context.Background(), &mbp.ReadRequest{Vault: vault, ID: id.String()})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(resp.KeyPoints) != 3 || resp.KeyPoints[1] != kpTargetPoint {
		t.Fatalf("ReadResponse.KeyPoints = %q", resp.KeyPoints)
	}
}

// Forget must drop key-point postings, or a forgotten engram stays reachable
// through a term only its key points contain.
func TestForget_RemovesKeyPointPostings(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "kp-forget"
	id := seedKPVault(t, eng, vault)
	ws := eng.store.ResolveVaultPrefix(vault)
	ctx := context.Background()

	hits := func() int {
		res, err := eng.fts.Search(ctx, ws, "heron backfill 41", 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.ID == [16]byte(id) {
				return 1
			}
		}
		return 0
	}
	if hits() != 1 {
		t.Fatal("precondition: key-point term not indexed")
	}
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: id.String()}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if hits() != 0 {
		t.Fatal("forgotten engram still has key-point postings")
	}
}

// A retag rewrites the engram's postings in place; it must keep the key-point
// postings, including those of terms the key points share with the content.
func TestUpdateTags_KeepsKeyPointPostings(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "kp-retag"
	id := seedKPVault(t, eng, vault)
	ws := eng.store.ResolveVaultPrefix(vault)
	ctx := context.Background()

	// "signed" is in the long content AND in a key point: the key-point side
	// is what lifts this long body, so losing that posting lowers the score.
	score := func() float64 {
		res, err := eng.fts.Search(ctx, ws, "everyone signed off", 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.ID == [16]byte(id) {
				return r.Score
			}
		}
		return 0
	}
	before := score()
	if before <= 0 {
		t.Fatal("precondition: target not found")
	}
	// Every query term is in a key point and the body is longer than this
	// vault's average, so the score is carried by the key-point side and a
	// retag (which only moves the content's length normaliser) must leave it
	// exactly where it was. Two retags: the second rewrites postings the
	// first already rewrote.
	for _, tags := range [][]string{{"alpha"}, {"bravo"}} {
		if err := eng.UpdateTags(ctx, vault, id, tags); err != nil {
			t.Fatal(err)
		}
		if after := score(); after < before-1e-9 {
			t.Fatalf("retag %v lost key-point postings: score %.6f -> %.6f", tags, before, after)
		}
	}
}

// Key points stored before this change (every enriched engram in an existing
// vault) are not in the index; reindex-fts is the backfill.
func TestReindexFTSVault_IndexesStoredKeyPoints(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "kp-backfill"
	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "c", Content: "plain words only"})
	if err != nil {
		t.Fatal(err)
	}
	awaitFTS(t, eng)
	id, _ := storage.ParseULID(resp.ID)
	// The pre-G2 write: the record gets key points, the index does not.
	if err := eng.store.UpdateDigest(ctx, id, "s", []string{"The pelican roster changed in March."}, "", ""); err != nil {
		t.Fatal(err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	found := func() bool {
		res, _ := eng.fts.Search(ctx, ws, "pelican roster", 10)
		for _, r := range res {
			if r.ID == [16]byte(id) {
				return true
			}
		}
		return false
	}
	if found() {
		t.Fatal("precondition: raw store write should not have indexed key points")
	}
	if _, err := eng.ReindexFTSVault(ctx, vault); err != nil {
		t.Fatalf("ReindexFTSVault: %v", err)
	}
	if !found() {
		t.Fatal("reindex-fts did not index stored key points")
	}
}
