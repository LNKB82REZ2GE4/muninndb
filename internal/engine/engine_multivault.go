package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// sharedBFSBudget is the total BFS traversal node budget ActivateMulti splits
// across the requested vaults. A single activateCore call caps its traversal
// at 500 nodes (architecture.md §3); a naive N-vault loop would grant each
// vault its own full 500-node budget (N×500 total), so ActivateMulti divides
// one shared budget instead.
const sharedBFSBudget = 500

// rrfK is the Reciprocal Rank Fusion rank-offset constant (score = weight /
// (rrfK + rank)). Cormack et al. 2009 use k=60 for web-scale result sets;
// memory recall result sets here are far smaller (single digits to low tens
// per vault), so a smaller k is used to keep rank differences meaningful
// instead of flattening them toward a near-uniform score.
const rrfK float64 = 10

// vaultRun pairs a vault's resolved ActivateResponse with the vault name that
// produced it (ActivationItem.Vault is also set, but keeping the name
// alongside the response avoids re-deriving it from items that may be empty).
type vaultRun struct {
	vault string
	resp  *mbp.ActivateResponse
}

// ActivateMulti runs activateCore once per vault (serially, sharing the one
// underlying Pebble store — parallelism across vaults is a later optimisation,
// not a correctness requirement) and merges the per-vault result sets by
// weighted Reciprocal Rank Fusion: score(e) = weight_vault / (rrfK +
// rank_in_vault(e)), highest first.
//
// Every reqs[i].Vault must already have passed the caller's key-scope check —
// ActivateMulti performs no authorization itself; callers (MCP/REST/gRPC) are
// responsible for rejecting the whole call if any requested vault is out of
// scope. len(weights) must equal len(reqs); callers wanting equal weighting
// should pass 1/N for every entry.
//
// Learning-signal handling (docs/plans/2026-07-19-project-vaults.md §4.2):
//   - Merging is by rank (RRF), not raw score: ACT-R/CGDN scores are computed
//     against vault-scoped BM25 corpus statistics and are not comparable
//     across vaults with very different sizes.
//   - PAS transition recording and Hebbian co-activation submission are only
//     allowed to run for the top-weighted vault (ties broken by first
//     occurrence). Every other vault is activated with its context tagged
//     observe-mode, which the engine already uses to suppress those write
//     side-effects for pure reads (see auth.ObserveFromContext call sites in
//     activateCore) — this avoids recording N separate PAS transition sets
//     for one logical multi-vault recall, and guarantees Hebbian boosts are
//     always fed a single vault's raw, non-fused score.
//   - A vault whose established embedding dimension differs from another
//     requested vault's is still merged in (not dropped), but is listed in
//     the response's DegradedVaults instead of being silently blended as if
//     fully comparable to vector-scored vaults.
func (e *Engine) ActivateMulti(ctx context.Context, reqs []*mbp.ActivateRequest, weights []float64) (*mbp.ActivateResponse, error) {
	if len(reqs) == 0 {
		return nil, fmt.Errorf("activate multi: at least one vault request is required")
	}
	if len(weights) != len(reqs) {
		return nil, fmt.Errorf("activate multi: weights must have the same length as vaults (%d != %d)", len(weights), len(reqs))
	}

	limit := reqs[0].MaxResults
	if limit <= 0 {
		limit = 10
	}

	perVaultBudget := sharedBFSBudget / len(reqs)
	if perVaultBudget < 1 {
		perVaultBudget = 1
	}

	topIdx := 0
	for i := 1; i < len(weights); i++ {
		if weights[i] > weights[topIdx] {
			topIdx = i
		}
	}

	// Dimension-mismatch degradation check: a vault whose established
	// embedding dimension differs from another requested vault's cannot be
	// meaningfully blended by semantic score — flag it rather than silently
	// mixing BM25-only results in alongside vector-scored ones.
	dims := make([]int, len(reqs))
	refDim := 0
	for i, r := range reqs {
		dims[i] = e.GetVaultEmbedDim(ctx, r.Vault)
		if refDim == 0 && dims[i] > 0 {
			refDim = dims[i]
		}
	}
	var degradedVaults []string
	for i, d := range dims {
		if d > 0 && refDim > 0 && d != refDim {
			degradedVaults = append(degradedVaults, reqs[i].Vault)
		}
	}

	runs := make([]vaultRun, len(reqs))
	var totalFound int
	for i, req := range reqs {
		callReq := *req
		callReq.BFSBudget = perVaultBudget
		callCtx := ctx
		if i != topIdx {
			// Non-top vaults are pure reads for this call: no PAS transition
			// recording, no Hebbian co-activation submission. See doc comment.
			callCtx = context.WithValue(ctx, auth.ContextMode, auth.ModeObserve)
		}
		resp, err := e.activateCore(callCtx, &callReq, nil)
		if err != nil {
			return nil, fmt.Errorf("activate multi: vault %q: %w", req.Vault, err)
		}
		for j := range resp.Activations {
			resp.Activations[j].Vault = req.Vault
		}
		runs[i] = vaultRun{vault: req.Vault, resp: resp}
		totalFound += resp.TotalFound
	}

	merged := mergeRRF(runs, weights, limit, rrfK)

	resp := &mbp.ActivateResponse{
		QueryID:        e.fastQueryID(),
		TotalFound:     totalFound,
		Activations:    merged,
		DegradedVaults: degradedVaults,
	}

	// Recompute the brief over the merged set, reusing the top-weighted
	// vault's query embedding when available. If no embedding is available
	// (or brief_mode explicitly disables it), leave the brief empty rather
	// than mixing separately-computed per-vault briefs.
	topReq := reqs[topIdx]
	if len(merged) > 0 && e.embedder != nil && len(topReq.Embedding) > 0 &&
		(topReq.BriefMode == "" || topReq.BriefMode == "auto" || topReq.BriefMode == "extractive") {
		resp.Brief = e.generateEmbeddingBrief(ctx, merged, topReq.Embedding)
	}

	return resp, nil
}

// rrfCandidate is one item under consideration during RRF merge.
type rrfCandidate struct {
	item  mbp.ActivationItem
	score float64
	vault string
}

// mergeRRF fuses per-vault ranked result sets by weighted Reciprocal Rank
// Fusion: score(e) = weight_vault / (k + rank_in_vault(e)), 1-based rank.
// Results are cut to limit, but every non-empty vault is guaranteed at least
// one slot (a floor) so a small/quiet vault's single relevant result is never
// crowded out entirely by a much larger vault's results.
func mergeRRF(runs []vaultRun, weights []float64, limit int, k float64) []mbp.ActivationItem {
	var all []rrfCandidate
	floor := make([]rrfCandidate, 0, len(runs))
	for i, run := range runs {
		w := weights[i]
		for rank, item := range run.resp.Activations {
			c := rrfCandidate{item: item, score: w / (k + float64(rank+1)), vault: run.vault}
			all = append(all, c)
			if rank == 0 {
				floor = append(floor, c)
			}
		}
	}

	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}

	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })

	key := func(c rrfCandidate) string { return c.vault + "|" + c.item.ID }

	selected := make([]rrfCandidate, 0, limit)
	seen := make(map[string]bool, limit)
	for _, c := range floor {
		if len(selected) >= limit {
			break
		}
		selected = append(selected, c)
		seen[key(c)] = true
	}
	for _, c := range all {
		if len(selected) >= limit {
			break
		}
		k := key(c)
		if seen[k] {
			continue
		}
		seen[k] = true
		selected = append(selected, c)
	}

	sort.SliceStable(selected, func(i, j int) bool { return selected[i].score > selected[j].score })

	items := make([]mbp.ActivationItem, len(selected))
	for i, c := range selected {
		items[i] = c.item
	}
	return items
}
