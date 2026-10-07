package engine

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"sort"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	// entityBoostFactor is the maximum score contribution of one shared
	// entity at peak rarity. The effective contribution of an entity is
	// entityBoostFactor × idf(entity) — see entityIDF — so ubiquitous
	// entities contribute ~0 and only rare shared entities carry weight
	// (issue #569).
	entityBoostFactor = float64(0.15)

	// entityBoostTopN is the number of top BFS results whose entity links are
	// used as seeds for the spread-activation pass.
	entityBoostTopN = 5

	// entityBoostCap bounds the total entity boost a single engram can
	// accumulate in one recall, no matter how many entities it shares with
	// the seeds. Entity co-mention is associative evidence; the cap keeps it
	// from outranking content evidence (issue #569).
	//
	// The cap deliberately sits at the LOW end of the BFS association weight
	// range (~0.3–0.9): two or more maximally-rare shared entities are allowed
	// to tie the weakest genuine graph edge, never a mid-strength one.
	//
	// CALIBRATION CAVEAT: factor, cap, and noise floor are calibrated to the
	// ACT-R score scale ([0, 1]). Under RRF fusion (scoring_fusion="rrf"),
	// content scores are rank-based and typically land in [0, 0.05]. On the
	// activateCore path the request threshold is defaulted to 0.1 before
	// activation.Run, so under RRF defaults no result clears the seed rule
	// and the boost pass is effectively a no-op. A caller-explicit small
	// threshold (e.g. 0.01) re-enables it, and there an injected engram can
	// enter well above genuine RRF content matches while the threshold gate
	// loses most of its bite. Scaling the boost to the active fusion mode is
	// a scoped follow-up (see PR #570 review); until then RRF callers should
	// treat entity-boosted scores as cross-scale.
	entityBoostCap = float64(0.30)

	// entityBoostNoiseFloor is the smallest per-entity contribution worth
	// pursuing. Entities below it (i.e. near-ubiquitous ones) are skipped
	// before their reverse index is scanned — they cannot meaningfully move
	// a score, and hub entities are exactly the ones with the largest scan
	// fan-out.
	entityBoostNoiseFloor = 0.001

	// entityBoostSourceMargin is how far below its source seed a boosted
	// neighbour is held (see the source bound in applyEntityBoost). It is a
	// REPRESENTATION margin, not a tuned weight: it must exceed the coarsest
	// precision Score is carried at (MCP rounds to 1e-6; float32 on MBP is
	// finer) so the bound survives the wire as a strict inequality, and stay
	// below entityBoostNoiseFloor so it never removes a boost worth applying.
	// Pinned by TestEntityBoost_SourceMarginSurvivesTheWire.
	entityBoostSourceMargin = 1e-5
)

// entityIDF returns the inverse-document-frequency weight for an entity:
// ln(n/df) / ln(n), clamped to [0, 1]. n is the RECALLED VAULT's engram count
// (vault-local — the flood #569 guards against is a vault-local phenomenon)
// while df is the entity's store-wide mention count (EntityRecords are deduped
// by name across vaults) — both maintained counters, no scans.
//
// The n-local/df-global split is deliberate and degrades safely: an entity
// unique to the vault gets an exactly-correct idf (the common case), while an
// entity shared across vaults gets UNDER-credited — df can approach or exceed
// the local n, shrinking idf or zeroing it via the df >= n guard. That is a
// false negative (rare shared entity missing some credit), never a re-flood,
// and unlike a global n it cannot grow with unrelated vaults added to the
// deployment. A true vault-scoped df needs per-vault mention counters on
// EntityRecord (a schema change touching every increment/decrement site) and
// is deliberately left to a follow-up.
//
// A rare entity (df ≪ n) approaches 1; an entity mentioned by most engrams
// approaches 0, so sharing it carries no associative evidence.
func entityIDF(df, n int64) float64 {
	if n < 2 || df < 1 || df >= n {
		return 0
	}
	return math.Log(float64(n)/float64(df)) / math.Log(float64(n))
}

// applyEntityBoost performs a post-BFS spread-activation pass using named
// entities. It takes the top-N results from the BFS activation, collects
// every entity linked to those engrams via the 0x20 forward index, then
// finds the other engrams in the result set that mention those entities via
// the 0x23 reverse index.
//
// Each distinct shared entity contributes entityBoostFactor × idf(entity)
// exactly once per target engram (regardless of how many seeds carry it —
// the evidence is the shared entity, not the seed count), and the total
// boost per engram is capped at entityBoostCap.
//
// Two structural bounds, both measured: on a 907-fact vault with
// free-text entities, adding entities cost 0.17 MRR over 60 paraphrased
// questions (21 lost, 2 won), and these bounds recover it to within noise of
// the entity-free baseline while removing every entity-only row from
// off-topic queries:
//
//  1. SOURCE BOUND. A boosted row is held strictly below the strongest seed
//     that contributed an entity to it (by entityBoostSourceMargin). Seeds are
//     never boosted themselves, so without this bound the pass promoted a
//     seed's neighbours OVER the seed that justified them — in 20 of the 21
//     losses the right answer was that seed. Spread activation cannot exceed
//     its source. The bound only ever withholds boost: a row is never lowered.
//
//  2. NO INJECTION. Only rows the pipeline returned are re-scored; an engram
//     phase 6 did not return is never appended. An entity-only row has no
//     measured aboutness for the query (zero content components, banded
//     "uncalibrated"/"not_scored"), and on off-topic queries such rows were
//     the entire response — every result at the boost value, burying the
//     weak genuine match and defeating abstention. This also retires the
//     side door #569 had to gate (meta filters, trust, lease visibility,
//     valid time): a row phase 6 admitted already passed that contract.
//
// Boosted rows carry the APPLIED boost in Components.EntityBoost (and Final
// tracks Score) so the adjustment is auditable.
//
// Results are re-sorted by score descending.
//
// vaultSize is the recalled vault's engram count (activateCore already holds
// it); it feeds entityIDF's n so rarity is judged against the vault being
// recalled from, not the whole deployment — see entityIDF.
func (e *Engine) applyEntityBoost(ctx context.Context, ws [8]byte, vaultSize int64, results []activation.ScoredEngram, req *activation.ActivateRequest) []activation.ScoredEngram {
	if len(results) == 0 {
		return results
	}

	// COG-18: the boost pass is explicitly skipped under rrf fusion. Its
	// factor/cap/noise-floor (see the CALIBRATION CAVEAT above) are calibrated
	// to the ACT-R [0, 1] scale; rrf finals are rank-based and typically land
	// in [0, 0.05]. Before R1 this was an accidental no-op — the pre-#590-fix
	// 0.1 threshold coerce meant no rrf result ever cleared the seed rule.
	// Now that default rrf recall reaches its genuine ~0.001 threshold
	// (activation.Run()'s mode-aware default), leaving this pass live would
	// let a +0.30-scale boost dominate and outrank real ~0.05-scale content
	// matches — an injected engram could out-score every genuine hit. Skip it
	// deliberately until the boost is recalibrated for rrf's scale (#570
	// follow-up); today's effective (accidental) behavior is preserved on
	// purpose.
	if req.Weights != nil && req.Weights.UseRRFFusion {
		slog.Debug("applyEntityBoost: skipped under rrf fusion (scale mismatch, #570 follow-up)")
		return results
	}

	// Seed the boost from at most entityBoostTopN top results, but ONLY from
	// results that genuinely cleared the relevance threshold. A result whose
	// Score is below the threshold is in the set only because it matched an
	// explicit tag filter (the S1 threshold bypass surfaces due:<=today style
	// reminders regardless of content relevance) — its named entities must not
	// become spread-activation seeds, or a content-unrelated reminder would drag
	// arbitrary entity-linked engrams into recall through a side door. results is
	// score-sorted descending, so above-threshold results are the prefix; we take
	// the top-N of those.
	seeds := make([]activation.ScoredEngram, 0, entityBoostTopN)
	for _, r := range results {
		if len(seeds) >= entityBoostTopN {
			break
		}
		if r.Score < req.Threshold {
			continue
		}
		seeds = append(seeds, r)
	}

	seedIDs := make(map[storage.ULID]struct{}, len(seeds))
	for _, s := range seeds {
		seedIDs[s.Engram.ID] = struct{}{}
	}

	n := vaultSize

	// Only rows the pipeline returned can be boosted (no injection), so the
	// reverse-index walk credits nothing outside this set.
	seenInResults := make(map[storage.ULID]int, len(results))
	for i, r := range results {
		seenInResults[r.Engram.ID] = i
	}

	// Pass 1: accumulate rarity-weighted boosts per target engram.
	type boostAcc struct {
		total   float64
		counted map[string]struct{} // entities already credited to this target
		// bound is the Score of the strongest seed that contributed to this
		// target. Seeds are visited in descending score order and each target's
		// accumulator is created on its first credit, so the first seed to
		// credit a target is its strongest source.
		bound float64
	}
	boosts := make(map[storage.ULID]*boostAcc)
	idfCache := make(map[string]float64, 8)
	scanned := make(map[string]struct{}, 8) // entities whose reverse index was already walked

	for _, seed := range seeds {
		_ = e.store.ScanEngramEntities(ctx, ws, seed.Engram.ID, func(entityName string) error {
			idf, cached := idfCache[entityName]
			if !cached {
				idf = 0
				if rec, err := e.store.GetEntityRecord(ctx, ws, entityName); err == nil && rec != nil {
					idf = entityIDF(int64(rec.MentionCount), n)
				}
				idfCache[entityName] = idf
			}
			contribution := entityBoostFactor * idf
			if contribution < entityBoostNoiseFloor {
				return nil // ubiquitous entity — no evidence; skip its fan-out
			}
			// Credit is once-per-entity per target, so a second seed carrying
			// the same entity cannot change any accumulator — skip the rescan.
			if _, done := scanned[entityName]; done {
				return nil
			}
			scanned[entityName] = struct{}{}
			// For each engram mentioning this entity (0x23 reverse index).
			return e.store.ScanEntityEngrams(ctx, entityName, func(entityWS [8]byte, engramID storage.ULID) error {
				if entityWS != ws {
					return nil // skip other vaults
				}
				if _, isSeed := seedIDs[engramID]; isSeed {
					return nil // seeds keep their BFS scores
				}
				if _, inResults := seenInResults[engramID]; !inResults {
					return nil // never injected — see the doc comment
				}
				acc := boosts[engramID]
				if acc == nil {
					acc = &boostAcc{counted: make(map[string]struct{}, 2), bound: seed.Score}
					boosts[engramID] = acc
				}
				if _, done := acc.counted[entityName]; done {
					return nil // this entity already credited to this target
				}
				acc.counted[entityName] = struct{}{}
				acc.total = math.Min(acc.total+contribution, entityBoostCap)
				return nil
			})
		})
	}

	if len(boosts) == 0 {
		return results
	}

	// Copy before mutating: results is the slice activation.Run returned, and
	// Run's async log-drain goroutine may still be reading that backing array
	// (the same reason activateCore's validity gate filters into a new slice).
	// Re-scoring and re-sorting in place is a data race; -race caught it once
	// this pass started re-scoring rows on the Activate path.
	results = append([]activation.ScoredEngram(nil), results...)

	// Pass 2: boost each credited row, held strictly below its source seed.
	for id, acc := range boosts {
		idx := seenInResults[id]
		room := acc.bound - entityBoostSourceMargin - results[idx].Score
		applied := math.Min(acc.total, room)
		if applied <= 0 {
			continue // no room below the source seed: leave the row untouched
		}
		results[idx].Score += applied
		results[idx].Components.EntityBoost = applied
		// Keep the reported Final consistent with the adjusted Score.
		results[idx].Components.Final = results[idx].Score
	}

	// Deterministic order (#698): SliceStable on Score alone is NOT enough here —
	// boosted rows can land on equal scores (equal entityBoostFactor·idf, or two
	// neighbours held at the same source bound), and their relative order would
	// then depend on the input order. Break ties by ULID so the output is
	// independent of it.
	// Mirrors applySupersession's tiebreak (engine_supersession.go).
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		a, b := results[i].Engram.ID, results[j].Engram.ID
		return bytes.Compare(a[:], b[:]) < 0
	})
	return results
}
