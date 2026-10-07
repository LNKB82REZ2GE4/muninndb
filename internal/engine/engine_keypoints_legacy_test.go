package engine

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// legacyKPVault: engram A carries key points on its RECORD that were never
// indexed (written before key points were indexed); B is uninvolved and owns
// the rare term those legacy key points share.
func legacyKPVault(t *testing.T, eng *Engine, vault string) (a, b storage.ULID) {
	t.Helper()
	ctx := context.Background()
	wa, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "a", Content: "alpha note about hedges"})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "b", Content: "the zarquon reactor manual"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "f", Content: "notes about gardens " + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	awaitFTS(t, eng)
	a, _ = storage.ParseULID(wa.ID)
	b, _ = storage.ParseULID(wb.ID)
	if err := eng.store.UpdateDigest(ctx, a, "", []string{"zarquon calibration was completed"}, "", ""); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func ftsScoreOf(t *testing.T, eng *Engine, vault, q string, id storage.ULID) float64 {
	t.Helper()
	res, err := eng.fts.Search(context.Background(), eng.store.ResolveVaultPrefix(vault), q, 50)
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

// Re-enriching a legacy engram must not decrement a DF another engram owns.
func TestUpdateDigestIndexed_LegacyKeyPointsDoNotShiftOtherScores(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "kp-legacy-reenrich"
	a, b := legacyKPVault(t, eng, vault)
	before := ftsScoreOf(t, eng, vault, "zarquon gardens", b)
	if err := eng.UpdateDigestIndexed(context.Background(), a, "", []string{"hedges were trimmed"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if after := ftsScoreOf(t, eng, vault, "zarquon gardens", b); math.Abs(after-before) > 1e-9 {
		t.Fatalf("uninvolved engram's score moved on a re-enrich: %.6f -> %.6f", before, after)
	}
}

// Retagging a legacy engram must not write postings for its never-indexed key
// points without counting them in DF (which over-credits other engrams). The
// legacy key points stay unindexed until reindex-fts, and nothing else moves.
func TestUpdateTags_LegacyKeyPointsStayConsistent(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "kp-legacy-retag"
	a, b := legacyKPVault(t, eng, vault)
	before := ftsScoreOf(t, eng, vault, "zarquon gardens", b)
	if err := eng.UpdateTags(context.Background(), vault, a, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if after := ftsScoreOf(t, eng, vault, "zarquon gardens", b); math.Abs(after-before) > 1e-9 {
		t.Fatalf("retag of a legacy engram moved an uninvolved score: %.6f -> %.6f", before, after)
	}
	if s := ftsScoreOf(t, eng, vault, "zarquon calibration", a); s != 0 {
		t.Fatalf("retag indexed legacy key points without DF bookkeeping (score %.6f)", s)
	}
}

// Forget racing a key-point write must not leave the forgotten engram
// reachable through the new key points.
func TestForget_RacingKeyPointWriteLeavesNothingReachable(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "kp-forget-race"
	reachable := 0
	const trials = 400
	for i := range trials {
		w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "c", Content: "plain words"})
		if err != nil {
			t.Fatal(err)
		}
		awaitFTS(t, eng)
		id, _ := storage.ParseULID(w.ID)
		term := "quokkaterm" + strings.Repeat("x", i%7) + string(rune('a'+i%26))
		done := make(chan struct{})
		go func() {
			_ = eng.UpdateDigestIndexed(ctx, id, "s", []string{"The " + term + " ledger closed."}, "", "")
			close(done)
		}()
		if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: w.ID}); err != nil {
			t.Fatal(err)
		}
		<-done
		if ftsScoreOf(t, eng, vault, term, id) > 0 {
			reachable++
		}
	}
	if reachable != 0 {
		t.Fatalf("forgotten engrams still reachable through a key point: %d/%d", reachable, trials)
	}
}
