package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// TestEntityBoost_ApplyEntityBoostDirect tests the applyEntityBoost helper
// directly, bypassing the full activation pipeline. B is a returned row that
// shares A's entity, sits well below A, and receives entityBoostFactor × idf;
// C is a returned row with a different entity and is untouched.
func TestEntityBoost_ApplyEntityBoostDirect(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-direct-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	err := eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
		Name:   "PostgreSQL",
		Type:   "database",
		Source: "inline",
	}, "inline")
	require.NoError(t, err)
	idA := writeLinkedEngram(t, eng, ws, "db-a", "PostgreSQL")
	idB := writeLinkedEngram(t, eng, ws, "db-b", "PostgreSQL")
	idC := writeLinkedEngram(t, eng, ws, "cache-c", "Redis")

	get := func(id storage.ULID) *storage.Engram {
		e, err := eng.store.GetEngram(ctx, ws, id)
		require.NoError(t, err)
		require.NotNil(t, e)
		return e
	}
	// A is the only seed (B and C are below the 0.3 threshold).
	initialResults := []activation.ScoredEngram{
		{Engram: get(idA), Score: 0.8},
		{Engram: get(idB), Score: 0.2},
		{Engram: get(idC), Score: 0.1},
	}

	// "PostgreSQL" was upserted once (df=1, n=3): idf = ln(3/1)/ln(3) = 1.0.
	boosted := eng.applyEntityBoost(ctx, ws, 3, initialResults, &activation.ActivateRequest{Threshold: 0.3})
	require.Len(t, boosted, 3, "the pass re-scores returned rows; it never adds one")

	byID := make(map[storage.ULID]activation.ScoredEngram, len(boosted))
	for _, r := range boosted {
		byID[r.Engram.ID] = r
	}
	require.InDelta(t, 0.8, byID[idA].Score, 1e-12, "the seed keeps its score")
	want := entityBoostFactor * entityIDF(1, 3)
	require.InDelta(t, 0.2+want, byID[idB].Score, 1e-9, "B gains entityBoostFactor × idf")
	require.InDelta(t, want, byID[idB].Components.EntityBoost, 1e-9, "the boost is auditable in Components.EntityBoost")
	require.InDelta(t, byID[idB].Score, byID[idB].Components.Final, 1e-12, "Final tracks the adjusted Score")
	require.InDelta(t, 0.1, byID[idC].Score, 1e-12, "C (different entity) is untouched")
	require.Zero(t, byID[idC].Components.EntityBoost)
}

// TestEntityBoost_BelowThresholdSeedDoesNotSpread guards the S1 interaction:
// the tag-filter threshold bypass admits content-unrelated due-reminders into
// the result set with a Score below the relevance threshold. Such a result must
// NOT act as a spread-activation seed, or its entities would lift arbitrary
// entity-linked rows. A is a below-threshold admission (0.02 < 0.05); B is a
// returned row sharing A's entity and must not be boosted.
func TestEntityBoost_BelowThresholdSeedDoesNotSpread(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-below-threshold-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	require.NoError(t, eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{Name: "Lawyer", Type: "person", Source: "inline"}, "inline"))
	idA := writeLinkedEngram(t, eng, ws, "due-a", "Lawyer")
	idB := writeLinkedEngram(t, eng, ws, "unrelated-b", "Lawyer")
	fullA, err := eng.store.GetEngram(ctx, ws, idA)
	require.NoError(t, err)
	fullB, err := eng.store.GetEngram(ctx, ws, idB)
	require.NoError(t, err)

	rows := func() []activation.ScoredEngram {
		return []activation.ScoredEngram{{Engram: fullA, Score: 0.02}, {Engram: fullB, Score: 0.01}}
	}
	boostOf := func(rs []activation.ScoredEngram) float64 {
		for _, r := range rs {
			if r.Engram.ID == idB {
				return r.Components.EntityBoost
			}
		}
		t.Fatalf("B missing from results")
		return 0
	}

	require.Zero(t, boostOf(eng.applyEntityBoost(ctx, ws, 2, rows(), &activation.ActivateRequest{Threshold: 0.05})),
		"a below-threshold admission must not seed entity boost")

	// Control: at threshold 0.015 A (0.02) is a legitimate seed and B (0.01) is
	// not, so B is boosted (up to A's bound) — proves the exclusion above is
	// what suppresses B, not a broken setup.
	require.Greater(t, boostOf(eng.applyEntityBoost(ctx, ws, 2, rows(), &activation.ActivateRequest{Threshold: 0.015})), 0.0,
		"control: with A above threshold, A seeds and B is boosted")
}

// TestEntityBoost_MaxResultsRespectedAfterBoost verifies that max_results is
// enforced, and score order preserved, after the entity boost phase re-sorts
// the result set. Regression test for issue #171.
func TestEntityBoost_MaxResultsRespectedAfterBoost(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "max-results-test"

	// Write one strong-match engram tagged with entity "PostgreSQL".
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "primary database",
		Content: "PostgreSQL primary relational database for transactional workloads",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
		},
	})
	require.NoError(t, err)

	// Write many additional entity-linked engrams; the entity boost phase
	// re-scores and re-sorts these after the BFS limit has been applied.
	for i := range 8 {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: "related config",
			Content: fmt.Sprintf("PostgreSQL related engram %d configuration details", i),
			Entities: []mbp.InlineEntity{
				{Name: "PostgreSQL", Type: "database"},
			},
		})
		require.NoError(t, err)
	}

	const maxResults = 3
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"PostgreSQL database"},
		MaxResults: maxResults,
	})
	require.NoError(t, err)
	require.LessOrEqual(t, len(resp.Activations), maxResults,
		"expected at most %d activations after entity boost, got %d", maxResults, len(resp.Activations))

	// Verify descending score order — entity boost re-sorts, truncation must preserve it.
	for i := 1; i < len(resp.Activations); i++ {
		if resp.Activations[i].Score > resp.Activations[i-1].Score {
			t.Errorf("activations not sorted descending at index %d: %.3f > %.3f",
				i, resp.Activations[i].Score, resp.Activations[i-1].Score)
		}
	}
}

// writeLinkedEngram writes an engram at the store level and links it to the
// given entities. Store-level writes bypass engine counters — tests using
// this helper pass the intended vault-local n to applyEntityBoost directly.
func writeLinkedEngram(t *testing.T, eng *Engine, ws [8]byte, concept string, entities ...string) storage.ULID {
	t.Helper()
	ctx := context.Background()
	id, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    concept,
		Content:    concept + " content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	for _, name := range entities {
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, id, name))
	}
	return id
}

// upsertEntityTimes upserts an entity record in vault ws `times` times, setting
// its vault-local MentionCount (df) to that value.
func upsertEntityTimes(t *testing.T, eng *Engine, ws [8]byte, name string, times int) {
	t.Helper()
	ctx := context.Background()
	for range times {
		require.NoError(t, eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
			Name:   name,
			Type:   "concept",
			Source: "inline",
		}, "inline"))
	}
}

// TestEntityBoost_UbiquitousEntityDoesNotFlood is the core regression for
// issue #569: engrams sharing only a ubiquitous entity with the seeds must
// not be injected into results, no matter how many seeds carry that entity.
// Before the fix, each of the 5 seeds contributed a flat 0.15 per shared
// entity, so hub engrams flooded recall at 5 × 0.15 = 0.75 with empty
// score components, evicting genuine matches at truncation.
func TestEntityBoost_UbiquitousEntityDoesNotFlood(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "flood-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Entity "Everyone" is mentioned by all 12 engrams: df = n → idf = 0.
	upsertEntityTimes(t, eng, ws, "Everyone", 12)
	ids := make([]storage.ULID, 12)
	for i := range ids {
		ids[i] = writeLinkedEngram(t, eng, ws, fmt.Sprintf("hub-engram-%d", i), "Everyone")
	}
	// Five seeds — the exact shape that produced 0.75 injections pre-fix.
	results := make([]activation.ScoredEngram, 0, 5)
	for i := 0; i < 5; i++ {
		full, err := eng.store.GetEngram(ctx, ws, ids[i])
		require.NoError(t, err)
		results = append(results, activation.ScoredEngram{Engram: full, Score: 0.8 - float64(i)*0.05})
	}

	boosted := eng.applyEntityBoost(ctx, ws, 12, results, &activation.ActivateRequest{Threshold: 0.1})

	require.Len(t, boosted, 5, "no engram may be injected on ubiquitous-entity evidence alone")
	for _, r := range boosted {
		require.LessOrEqual(t, r.Components.EntityBoost, entityBoostNoiseFloor,
			"a ubiquitous entity must contribute ~0 boost")
	}
}

// TestEntityBoost_SameEntityViaMultipleSeedsCountsOnce verifies that a
// shared entity is credited to a target once, regardless of how many seeds
// carry it — the evidence is the shared entity, not the seed count.
func TestEntityBoost_SameEntityViaMultipleSeedsCountsOnce(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "dedup-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	// "DupEnt" is mentioned by 3 engrams (2 seeds + 1 target) in a corpus of 100.
	upsertEntityTimes(t, eng, ws, "DupEnt", 3)
	seed1 := writeLinkedEngram(t, eng, ws, "dedup-seed-1", "DupEnt")
	seed2 := writeLinkedEngram(t, eng, ws, "dedup-seed-2", "DupEnt")
	target := writeLinkedEngram(t, eng, ws, "dedup-target", "DupEnt")

	fullSeed1, err := eng.store.GetEngram(ctx, ws, seed1)
	require.NoError(t, err)
	fullSeed2, err := eng.store.GetEngram(ctx, ws, seed2)
	require.NoError(t, err)

	fullTarget, err := eng.store.GetEngram(ctx, ws, target)
	require.NoError(t, err)

	boosted := eng.applyEntityBoost(ctx, ws, 100, []activation.ScoredEngram{
		{Engram: fullSeed1, Score: 0.8},
		{Engram: fullSeed2, Score: 0.7},
		{Engram: fullTarget, Score: 0.01},
	}, &activation.ActivateRequest{Threshold: 0.05})

	expected := entityBoostFactor * entityIDF(3, 100)
	var targetBoost float64
	found := false
	for _, r := range boosted {
		if r.Engram.ID == target {
			targetBoost = r.Components.EntityBoost
			found = true
		}
	}
	require.True(t, found, "target must stay in the results")
	require.InDelta(t, expected, targetBoost, 1e-6,
		"entity shared with two seeds must be credited once, not twice")
}

// TestEntityBoost_CapBoundsAccumulation verifies that the total boost a
// single engram accumulates across several rare shared entities is clamped
// at entityBoostCap.
func TestEntityBoost_CapBoundsAccumulation(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "cap-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	entities := []string{"CapE1", "CapE2", "CapE3", "CapE4"}
	for _, name := range entities {
		upsertEntityTimes(t, eng, ws, name, 2) // df=2 each — rare
	}
	seed := writeLinkedEngram(t, eng, ws, "cap-seed", entities...)
	target := writeLinkedEngram(t, eng, ws, "cap-target", entities...)

	// Sanity: uncapped accumulation would exceed the cap.
	perEntity := entityBoostFactor * entityIDF(2, 1000)
	require.Greater(t, 4*perEntity, entityBoostCap, "test setup must exceed the cap uncapped")

	fullSeed, err := eng.store.GetEngram(ctx, ws, seed)
	require.NoError(t, err)
	fullTarget, err := eng.store.GetEngram(ctx, ws, target)
	require.NoError(t, err)

	// The target sits far enough below the seed that the source bound does not
	// bind: only the cap can.
	boosted := eng.applyEntityBoost(ctx, ws, 1000, []activation.ScoredEngram{
		{Engram: fullSeed, Score: 0.9},
		{Engram: fullTarget, Score: 0.01},
	}, &activation.ActivateRequest{Threshold: 0.05})

	found := false
	for _, r := range boosted {
		if r.Engram.ID == target {
			found = true
			require.InDelta(t, entityBoostCap, r.Components.EntityBoost, 1e-9,
				"total boost must clamp at entityBoostCap")
			require.InDelta(t, 0.01+entityBoostCap, r.Score, 1e-9)
		}
	}
	require.True(t, found, "target must stay in the results")
}

// TestEntityBoost_TotalFoundCoversFilteredResults is the fingerprint
// regression from the production report on #569: with meta filters set, the
// boost pass appended unfiltered records after TotalFound was computed, so
// any response with total < len(activations) is the bug firing (observed:
// total=4 with 44 records returned). After the fix, every returned record
// passes the filter and TotalFound covers the returned set.
func TestEntityBoost_TotalFoundCoversFilteredResults(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "fingerprint-test"

	// One engram matches both the query and the tag filter.
	respWanted, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:    vault,
		Concept:  "wanted flow record",
		Content:  "PostgreSQL flow record with the wanted tag",
		Tags:     []string{"record_kind=flow"},
		Entities: []mbp.InlineEntity{{Name: "PostgreSQL", Type: "database"}},
	})
	require.NoError(t, err)

	// Entity-linked engrams WITHOUT the tag: phase 6 filters them out, so
	// pre-fix they re-entered via injection, past the filter and past
	// TotalFound.
	untaggedIDs := make(map[string]bool, 6)
	for i := range 6 {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:    vault,
			Concept:  fmt.Sprintf("untagged sibling %d", i),
			Content:  fmt.Sprintf("PostgreSQL sibling record %d without the tag", i),
			Entities: []mbp.InlineEntity{{Name: "PostgreSQL", Type: "database"}},
		})
		require.NoError(t, err)
		untaggedIDs[resp.ID] = true
	}

	// Filler engrams without the entity keep "PostgreSQL" rare enough that
	// its idf — and thus the injection evidence — stays above threshold
	// (df == n would zero the boost and the test would pass vacuously).
	for i := range 10 {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("filler %d", i),
			Content: fmt.Sprintf("Redis cache entry %d for session data", i),
		})
		require.NoError(t, err)
	}

	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"PostgreSQL flow record"},
		MaxResults: 50,
		Threshold:  0.01,
		Filters:    []mbp.Filter{{Field: "tags_all", Value: []string{"record_kind=flow"}}},
	})
	require.NoError(t, err)

	// No unfiltered record may be returned.
	foundWanted := false
	for _, item := range resp.Activations {
		require.False(t, untaggedIDs[item.ID],
			"engram %s (no required tag) leaked into tag-filtered results via entity boost", item.ID)
		if item.ID == respWanted.ID {
			foundWanted = true
		}
	}
	require.True(t, foundWanted, "the tagged engram must be returned")

	// The fingerprint: any response with total < len(activations) is the bug.
	require.GreaterOrEqual(t, resp.TotalFound, len(resp.Activations),
		"TotalFound must cover the returned set (fingerprint: total < len(activations))")

	// Control from the production report: a filter matching nothing returns
	// nothing. Note this passes even pre-fix (no survivors → no seeds → the
	// boost early-returns), which is why it cannot serve as the regression
	// test on its own.
	empty, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"PostgreSQL flow record"},
		MaxResults: 50,
		Threshold:  0.01,
		Filters:    []mbp.Filter{{Field: "tags_all", Value: []string{"no-such-tag"}}},
	})
	require.NoError(t, err)
	require.Empty(t, empty.Activations, "filter matching nothing must return nothing")
}

// TestEntityBoost_RarityIsVaultLocal is the two-vaults-one-engine regression
// for the review on #570: entityIDF's n must be the recalled vault's engram
// count, not the deployment-global counter. With a global n, an entity that is
// locally ubiquitous (on every engram in its vault — what should score
// idf ≈ 0) regains a positive idf as unrelated vaults are added to the same
// server: idf(10, 10) = 0 but idf(10, 34) ≈ 0.35, which would boost every
// hub sibling. Driven through Activate so activateCore's vaultSize wiring is
// what is tested.
//
// Shape: the seed and four decoys match the query on every term and fill the
// five seed slots; the hub siblings and the rare sibling match on one term, so
// they are returned as non-seed rows the pass can boost. Every engram in vault
// A carries AlphaHub; only the seed and the rare sibling carry AlphaRare.
func TestEntityBoost_RarityIsVaultLocal(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultA = "vault-local-a"
	const vaultB = "vault-local-b"
	hub := mbp.InlineEntity{Name: "AlphaHub", Type: "concept"}
	rareEnt := mbp.InlineEntity{Name: "AlphaRare", Type: "concept"}

	write := func(vault, concept, content string, ents ...mbp.InlineEntity) string {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: concept, Content: content, Entities: ents})
		require.NoError(t, err)
		return resp.ID
	}

	seed := write(vaultA, "primary database choice",
		"We use PostgreSQL as the primary relational database for transactional workloads", hub, rareEnt)
	for i := range 4 {
		write(vaultA, fmt.Sprintf("decoy %d", i),
			fmt.Sprintf("primary relational database workloads decoy %d", i), hub)
	}
	hubOnly := make(map[string]bool, 4)
	for i := range 4 {
		hubOnly[write(vaultA, fmt.Sprintf("hub sibling %d", i),
			fmt.Sprintf("relational database moss shelf audit note %d", i), hub)] = true
	}
	rare := write(vaultA, "rare sibling", "relational database bench ledger of appeals", hub, rareEnt)

	// Vault B: unrelated engrams sharing the engine. Under a global n these
	// inflate every idf computed for vault A recalls.
	for i := range 24 {
		write(vaultB, fmt.Sprintf("unrelated %d", i), fmt.Sprintf("orchard weather almanac entry %d", i))
	}

	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vaultA,
		Context:    []string{"primary relational database workloads"},
		MaxResults: 20,
		Threshold:  0.01,
	})
	require.NoError(t, err)

	boost := make(map[string]float64, len(resp.Activations))
	for _, item := range resp.Activations {
		boost[item.ID] = float64(item.ScoreComponents.EntityBoost)
	}
	require.Contains(t, boost, seed, "the full-match seed must be returned")
	require.Contains(t, boost, rare, "the one-term rare sibling must be returned (fixture sanity)")
	require.Greater(t, boost[rare], 0.0,
		"a LOCALLY rare shared entity must still boost its sibling (idf(2,10) ≈ 0.30)")
	nHub := 0
	for id := range hubOnly {
		if b, ok := boost[id]; ok {
			nHub++
			// With the vault-local n the hub's idf is ~0 (the engine's vault
			// counter can run a little above the engrams written here, so it
			// is not exactly 0); a deployment-global n (34) gives idf ≈ 0.35.
			// Half the global-n boost discriminates the two with wide margin.
			require.Less(t, b, entityBoostFactor*entityIDF(10, 34)/2,
				"a LOCALLY ubiquitous entity (on every engram of the vault) must boost ~nothing, "+
					"no matter how many engrams other vaults hold")
		}
	}
	require.Positive(t, nHub, "fixture sanity: at least one hub sibling must be returned to be checked")
}
