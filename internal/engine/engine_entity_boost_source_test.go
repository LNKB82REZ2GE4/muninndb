package engine

import (
	"context"
	"math"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// Source-bounded entity boost. Measured on a 907-fact vault with
// free-text entities: 21 of 60 paraphrased questions lost rank when entities
// were added, and in 20 of the 21 the right answer was a SEED (a top-5 result
// that triggers the pass) overtaken by non-seed rows that had gained up to
// +0.30 by sharing that seed's entities. The seed itself is never boosted, so
// the pass systematically promoted the seed's neighbours over the seed that
// justified them. Spread activation cannot exceed its source: a boosted row
// must stay strictly below the strongest seed that contributed to it.

// TestEntityBoost_NeighbourNeverOvertakesItsSourceSeed is the RED test for the
// measured loss shape: answer seed at 0.40, a sibling at 0.25 sharing two
// rare entities with it (nominal boost 0.30 → 0.55). Before the fix the
// sibling outranks the seed.
func TestEntityBoost_NeighbourNeverOvertakesItsSourceSeed(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	ws := eng.store.ResolveVaultPrefix("g3-source-bound")
	upsertEntityTimes(t, eng, ws, "SrcE1", 2)
	upsertEntityTimes(t, eng, ws, "SrcE2", 2)
	seedID := writeLinkedEngram(t, eng, ws, "g3-seed", "SrcE1", "SrcE2")
	sibID := writeLinkedEngram(t, eng, ws, "g3-sibling", "SrcE1", "SrcE2")
	seed, err := eng.store.GetEngram(ctx, ws, seedID)
	require.NoError(t, err)
	sib, err := eng.store.GetEngram(ctx, ws, sibID)
	require.NoError(t, err)

	// Sanity: the nominal boost alone would carry the sibling past the seed.
	nominal := math.Min(2*entityBoostFactor*entityIDF(2, 1000), entityBoostCap)
	require.Greater(t, 0.25+nominal, 0.40, "fixture must exercise an overtake")

	// The answer seed is rank 1; four entity-less results fill the other seed
	// slots (entityBoostTopN = 5), so the sibling is the 6th row — exactly the
	// measured shape (boosted rows sat at ranks 6-10 before the boost).
	in := []activation.ScoredEngram{{Engram: seed, Score: 0.40}}
	for i := range 4 {
		id := writeLinkedEngram(t, eng, ws, "g3-other-"+string(rune('a'+i)))
		full, err := eng.store.GetEngram(ctx, ws, id)
		require.NoError(t, err)
		in = append(in, activation.ScoredEngram{Engram: full, Score: 0.30 - 0.01*float64(i)})
	}
	in = append(in, activation.ScoredEngram{Engram: sib, Score: 0.25})

	out := eng.applyEntityBoost(ctx, ws, 1000, in, &activation.ActivateRequest{Threshold: 0.1})

	require.Len(t, out, 6)
	require.Equal(t, seedID, out[0].Engram.ID, "the source seed must keep rank 1")
	var got activation.ScoredEngram
	for _, r := range out {
		if r.Engram.ID == sibID {
			got = r
		}
	}
	require.NotNil(t, got.Engram)
	require.Less(t, got.Score, out[0].Score, "a boosted neighbour must stay strictly below its source seed")
	require.Less(t, float32(got.Score), float32(out[0].Score),
		"the bound must survive the float32 wire conversion")
	// Still boosted within the room the seed leaves — the feature is bounded,
	// not amputated (#569/#581 doctrine). It may pass the OTHER seeds (0.27-0.30):
	// the bound is its own source, not every seed.
	require.Greater(t, got.Score, 0.30, "the neighbour keeps the boost that fits below its seed")
	require.InDelta(t, got.Score-0.25, got.Components.EntityBoost, 1e-12,
		"Components.EntityBoost must report the APPLIED boost, not the nominal one")
	require.InDelta(t, got.Score, got.Components.Final, 1e-12)
}

// TestEntityBoost_NoRoomMeansNoBoostAndNoDemotion: a neighbour already level
// with (or above) its source seed gets nothing — and is never lowered. Before
// the fix it received the full nominal boost.
func TestEntityBoost_NoRoomMeansNoBoostAndNoDemotion(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	ws := eng.store.ResolveVaultPrefix("g3-no-room")
	upsertEntityTimes(t, eng, ws, "RoomE", 2)
	// Six rows at one score: the first five (score-sorted) are seeds; the
	// sixth shares RoomE with seed #1 but sits exactly level with it.
	ids := make([]storage.ULID, 6)
	rows := make([]activation.ScoredEngram, 6)
	for i := range ids {
		if i == 0 || i == 5 {
			ids[i] = writeLinkedEngram(t, eng, ws, "room-"+string(rune('a'+i)), "RoomE")
		} else {
			ids[i] = writeLinkedEngram(t, eng, ws, "room-"+string(rune('a'+i)))
		}
		full, err := eng.store.GetEngram(ctx, ws, ids[i])
		require.NoError(t, err)
		rows[i] = activation.ScoredEngram{Engram: full, Score: 0.30}
	}
	out := eng.applyEntityBoost(ctx, ws, 1000, rows, &activation.ActivateRequest{Threshold: 0.1})
	require.Len(t, out, 6)
	for _, r := range out {
		require.Equal(t, 0.30, r.Score, "no row may be raised above, or lowered below, the level its seed allows")
		require.Zero(t, r.Components.EntityBoost)
	}
}

// TestEntityBoost_NeverInjects: the pass may re-score rows the pipeline
// returned, never add rows. An injected row carries no measured aboutness for
// the query (zero content components, band "uncalibrated"/"not_scored"), and
// on an off-topic query these rows were the whole response: the production
// "every result scores 0.3, entity_boost 0.3" shape. Before the fix the
// rare-entity target here was appended.
func TestEntityBoost_NeverInjects(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	ws := eng.store.ResolveVaultPrefix("g3-no-inject")
	upsertEntityTimes(t, eng, ws, "InjE1", 2)
	upsertEntityTimes(t, eng, ws, "InjE2", 2)
	seedID := writeLinkedEngram(t, eng, ws, "inj-seed", "InjE1", "InjE2")
	writeLinkedEngram(t, eng, ws, "inj-target", "InjE1", "InjE2") // NOT in the input set
	seed, err := eng.store.GetEngram(ctx, ws, seedID)
	require.NoError(t, err)

	for _, th := range []float64{0.0, 0.01, 0.05, 0.1} {
		out := eng.applyEntityBoost(ctx, ws, 1000, []activation.ScoredEngram{{Engram: seed, Score: 0.8}},
			&activation.ActivateRequest{Threshold: th})
		require.Len(t, out, 1, "threshold %.2f: an engram the pipeline did not return must never be injected", th)
		require.Equal(t, seedID, out[0].Engram.ID)
	}
}

// TestEntityBoost_ActivateReturnsNoEntityOnlyRows drives the real Activate
// path: an engram whose content shares nothing with the query, linked only by
// a rare entity to the query's match, must not appear, and no returned row may
// carry the not_scored basis an entity-only row is banded with. Before the fix
// the linked engram was returned (this was the original "surfaces
// entity-linked engram" behaviour).
func TestEntityBoost_ActivateReturnsNoEntityOnlyRows(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "g3-activate-no-filler"

	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:    vault,
		Concept:  "primary database choice",
		Content:  "We use PostgreSQL as the primary relational database for all transactional workloads",
		Entities: []mbp.InlineEntity{{Name: "PostgreSQL", Type: "database"}},
	})
	require.NoError(t, err)
	linked, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:    vault,
		Concept:  "replica configuration",
		Content:  "Read replica configuration for streaming replication failover setup",
		Entities: []mbp.InlineEntity{{Name: "PostgreSQL", Type: "database"}},
	})
	require.NoError(t, err)
	for i := range 5 { // keep PostgreSQL rare (df=2, n=7)
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: "filler",
			Content: "Redis is used as an in-memory cache for session data " + string(rune('a'+i)),
		})
		require.NoError(t, err)
	}
	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"primary relational database"},
		MaxResults: 20,
		Threshold:  0.01,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Activations, "sanity: the content match must be returned")
	for _, item := range resp.Activations {
		require.NotEqual(t, linked.ID, item.ID, "an entity-only engram must not be returned")
		require.NotEqual(t, activation.BasisNotScored, item.RelevanceBandBasis,
			"no returned row may be an unscored entity-boost injection")
	}
}

// TestEntityBoost_SourceMarginSurvivesTheWire pins entityBoostSourceMargin to
// the precision of the surfaces that carry Score: float32 on MBP, and
// 6-decimal rounding on MCP (internal/mcp/convert.go roundScore). A margin
// below either would let a bounded neighbour TIE its seed on the wire, and the
// ULID tiebreak (or a client re-sort) could then put it first. It must also
// stay negligible next to the boost itself.
func TestEntityBoost_SourceMarginSurvivesTheWire(t *testing.T) {
	t.Parallel()
	require.Greater(t, entityBoostSourceMargin, 1e-6, "must exceed MCP's 1e-6 score rounding")
	require.Less(t, entityBoostSourceMargin, entityBoostNoiseFloor,
		"must be smaller than the smallest per-entity contribution the pass applies")
	for s := 0.001; s <= 1.0; s += 0.0007 {
		b := s - entityBoostSourceMargin
		require.Less(t, float32(b), float32(s), "float32 must keep the bound below the seed at %.4f", s)
		require.Less(t, math.Round(b*1e6), math.Round(s*1e6), "6-dp rounding must keep the bound below the seed at %.4f", s)
	}
}
