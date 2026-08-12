package consolidation

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DreamOpts configures a dream consolidation pass.
type DreamOpts struct {
	DryRun bool
	Force  bool   // bypass trigger gates
	Scope  string // limit to single vault ("" = all vaults)
}

// DreamReport collects results across all vaults for a single dream run.
type DreamReport struct {
	Reports       []*ConsolidationReport
	Skipped       []string // vault names skipped (legal, no LLM, etc.)
	TotalDuration time.Duration
}

// DreamOnce runs a single dream consolidation pass across vaults.
// In dream mode, the dedup threshold is lowered to 0.85 to surface
// near-duplicate candidates for future LLM review (Phase 2b).
func (w *Worker) DreamOnce(ctx context.Context, opts DreamOpts) (*DreamReport, error) {
	start := time.Now()
	dreport := &DreamReport{}

	// TODO: enforce trigger gates (time >= 12h + volume >= 3 engrams) when Force is false.
	// ReadDreamState/WriteDreamState are implemented but gate logic is deferred to PR #2.

	// Resolve which vaults to process.
	var vaults []string
	if opts.Scope != "" {
		vaults = []string{opts.Scope}
	} else {
		var err error
		vaults, err = w.Engine.ListVaults(ctx)
		if err != nil {
			return nil, fmt.Errorf("dream: list vaults: %w", err)
		}
	}

	if len(vaults) == 0 {
		slog.Info("dream: no vaults to process")
		dreport.TotalDuration = time.Since(start)
		return dreport, nil
	}

	store := w.Engine.Store()

	// Construct a dream-specific worker to avoid mutating the caller's instance.
	// This prevents data races if DreamOnce is called while the background
	// consolidation scheduler is running on the same Worker.
	dw := &Worker{
		Engine:            w.Engine,
		Schedule:          w.Schedule,
		MaxDedup:          w.MaxDedup,
		MaxTransitive:     w.MaxTransitive,
		DryRun:            opts.DryRun,
		DedupThreshold:    0.85,
		MinDedupVaultSize: w.MinDedupVaultSize,
	}

	for _, vault := range vaults {
		if err := ctx.Err(); err != nil {
			return dreport, fmt.Errorf("dream: context cancelled: %w", err)
		}

		wsPrefix := store.ResolveVaultPrefix(vault)

		report := &ConsolidationReport{
			Vault:     vault,
			StartedAt: time.Now(),
			DryRun:    opts.DryRun,
		}

		// Phase 0: Orient
		summary, err := dw.runPhase0Orient(ctx, store, wsPrefix, vault)
		if err != nil {
			slog.Warn("dream: phase 0 (orient) failed", "vault", vault, "error", err)
			report.Errors = append(report.Errors, "phase0_orient: "+err.Error())
		}
		report.Orient = summary

		// Skip legal vaults entirely.
		if summary != nil && summary.IsLegal {
			report.LegalSkipped = summary.EngramCount
			slog.Info("dream: skipping legal vault (protected)",
				"vault", vault, "engrams", summary.EngramCount)
			dreport.Skipped = append(dreport.Skipped, vault)
			report.Duration = time.Since(report.StartedAt)
			dreport.Reports = append(dreport.Reports, report)
			continue
		}

		// Phase 1: Activation Replay
		if err := dw.runPhase1Replay(ctx, store, wsPrefix, report); err != nil {
			slog.Warn("dream: phase 1 (replay) failed", "vault", vault, "error", err)
			report.Errors = append(report.Errors, "phase1_replay: "+err.Error())
		}

		// Phase 2: Semantic Deduplication (threshold 0.85 in dream mode)
		//
		// Guard: skip dedup when the vault is too small. Removing even a single
		// cluster from a small vault can shift the per-query normalization anchor
		// in the ACT-R scoring path, flipping top-1 results for unrelated queries.
		// At MinDedupVaultSize (default 20) a 3-engram cluster removal represents
		// at most a 15% reduction; below this threshold the landscape is too
		// sensitive to dedup mutations to guarantee retrieval recall is preserved.
		// See: https://github.com/scrypster/muninndb/issues/311
		minSize := dw.MinDedupVaultSize
		if minSize <= 0 {
			minSize = 20
		}
		// embedCount is the number of engrams that actually participate in
		// dedup (Phase 2 operates only on embedding-bearing engrams). Using
		// WithEmbed rather than EngramCount avoids counting embed-less engrams
		// that would never affect the normalization anchor.
		// When summary is nil (Phase 0 failed), vault size is unknown — skip
		// dedup defensively rather than proceeding blind.
		embedCount := 0
		if summary != nil {
			embedCount = summary.WithEmbed
		}
		if summary == nil || embedCount < minSize {
			slog.Info("dream: skipping phase 2 dedup — vault below minimum size",
				"vault", vault, "engrams_with_embed", embedCount, "min", minSize)
		} else if err := dw.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
			slog.Warn("dream: phase 2 (dedup) failed", "vault", vault, "error", err)
			report.Errors = append(report.Errors, "phase2_dedup: "+err.Error())
		}

		// Phase 2b: LLM Consolidation (future PR)

		// Phase 3: Schema Promotion — boost hub engrams (>=10 out-edges,
		// relevance >=0.8) so load-bearing concepts resist fading. Wired into
		// the dream pass by the fork (upstream only ran it in the background
		// scheduler, which the server never starts).
		if err := dw.runPhase3SchemaPromotion(ctx, store, wsPrefix, report); err != nil {
			slog.Warn("dream: phase 3 (schema promotion) failed", "vault", vault, "error", err)
			report.Errors = append(report.Errors, "phase3_schema: "+err.Error())
		}

		// Phase 4: Decay Acceleration — deliberately NOT wired: its 30d/0.3/0.5
		// constants are hardcoded, which violates the per-vault calibration
		// principle; revisit only with per-vault knobs.

		// Phase 5: Transitive Inference — infer A→C where A→B and B→C are both
		// strong and no direct edge exists. Same fork-wiring rationale as phase 3.
		if err := dw.runPhase5TransitiveInference(ctx, store, wsPrefix, report); err != nil {
			slog.Warn("dream: phase 5 (transitive inference) failed", "vault", vault, "error", err)
			report.Errors = append(report.Errors, "phase5_transitive: "+err.Error())
		}

		report.Duration = time.Since(report.StartedAt)
		dreport.Reports = append(dreport.Reports, report)

		slog.Info("dream: vault completed", "vault", vault, "duration", report.Duration,
			"merged", report.MergedEngrams)
	}

	dreport.TotalDuration = time.Since(start)
	return dreport, nil
}
