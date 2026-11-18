package engine

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// Key points (G2) are the enricher's 3-7 self-contained statements about an
// engram, persisted on the record by UpdateDigest. They are indexed in their own
// FTS field (fts.FieldKeyPoints) so a recall query that matches ONE key point
// finds the engram, and recall presents the matched key point as the row's
// snippet (mbp.ActivationItem.MatchedKeyPoint).
//
// The invariant this file exists to keep: every path that writes, rewrites or
// removes an engram's FTS postings uses the SAME document, key points included —
// ftsDocument below. A delete that omits them orphans key-point postings (a
// forgotten engram stays reachable through them); a reindex that omits them
// drops the postings of every key-point term the edit retains.

// ftsDocument is the indexable document of a stored engram. Slices are copied:
// the record may be the L1 cache's live entry, and the document outlives it.
func ftsDocument(eng *storage.Engram) fts.Document {
	if eng == nil {
		return fts.Document{}
	}
	return fts.Document{
		Concept:   eng.Concept,
		CreatedBy: eng.CreatedBy,
		Content:   eng.Content,
		Tags:      append([]string(nil), eng.Tags...),
		KeyPoints: append([]string(nil), eng.KeyPoints...),
	}
}

// indexedPrevDocument is ftsDocument for the PREVIOUS state of an in-place
// reindex: key points that were never indexed (stored before G2, vault not yet
// through reindex-fts) are dropped, so ReindexEngram's membership-based DF
// bookkeeping sees exactly what the index holds.
func (e *Engine) indexedPrevDocument(ws [8]byte, eng *storage.Engram) fts.Document {
	d := ftsDocument(eng)
	if e.fts != nil && len(d.KeyPoints) > 0 && !e.fts.KeyPointsIndexed(ws, [16]byte(eng.ID), d.KeyPoints) {
		d.KeyPoints = nil
	}
	return d
}

// UpdateDigestIndexed persists an enrichment digest (store.UpdateDigest) and
// brings the engram's FTS key-point postings in step with the key points it
// wrote. It is the only way enrichment output should reach the store: the MCP
// apply path, replay, and the in-process enrich plugin (via its store adapter)
// all route here.
//
// The stripe lock serialises this against Engine.UpdateTags, the other in-place
// FTS rewrite of an existing engram, so neither can compute its "previous
// document" from a record the other is halfway through changing. Forget closes
// the race from its side on both branches: a SOFT forget also deletes the
// postings of a post-SoftDelete re-read, and a HARD forget de-indexes the
// record storage.DeleteEngramReturning read under this same stripe lock (not
// an unlocked pre-read, which missed key points indexed between the two — the
// soft-only wording this comment used to carry was wrong for hard forget).
//
// FTS is a derived index: a failed reindex is logged, not returned, exactly as
// in UpdateTags — the digest is already durable, and `muninn reindex-fts`
// rebuilds the postings from the records.
func (e *Engine) UpdateDigestIndexed(ctx context.Context, id storage.ULID, summary string, keyPoints []string, memoryType, typeLabel string) error {
	// It rewrites an EXISTING engram: refused for an append-only credential and
	// on a follower, as ApplyEnrichment / ReplayEnrichment are. Behaviour change
	// for the in-process enrich worker, which used to call store.UpdateDigest
	// with no gate: on a follower its digest writes are now refused instead of
	// landing locally and never replicating (the #596 class).
	if err := e.refuseWrite(ctx); err != nil {
		return err
	}
	unlock := e.store.LockEngram(id)
	defer unlock()

	ws, found := e.store.FindVaultPrefix(id)
	var prevDoc fts.Document
	havePrev := false
	if found {
		if prev, err := e.store.GetEngram(ctx, ws, id); err == nil && prev != nil {
			prevDoc, havePrev = e.indexedPrevDocument(ws, prev), true
		}
	}

	// Locked variant: this stripe is already held above (not reentrant).
	if err := e.store.UpdateDigestLocked(ctx, id, summary, keyPoints, memoryType, typeLabel); err != nil {
		return err
	}

	// UpdateDigest only replaces key points when some are given; nothing else it
	// writes is indexed.
	if e.fts == nil || len(keyPoints) == 0 {
		return nil
	}
	if !havePrev {
		slog.Warn("engine: update digest: pre-read failed, key points not indexed until reindex-fts",
			"id", id.String())
		return nil
	}
	cur, err := e.store.GetEngram(ctx, ws, id)
	if err != nil || cur == nil {
		slog.Warn("engine: update digest: post-read failed, key points not indexed until reindex-fts",
			"id", id.String(), "err", err)
		return nil
	}
	// Same lifecycle rule as UpdateTags: a forgotten (trashed) engram was
	// de-indexed by Forget and must stay out of the candidate pool; superseded
	// history keeps its postings.
	if cur.State == storage.StateSoftDeleted && !activation.CarriesSupersessionSignature(cur) {
		return nil
	}
	next := ftsDocument(cur)
	if slices.Equal(prevDoc.KeyPoints, next.KeyPoints) {
		return nil
	}
	// Only the key points changed under this lock; every other field of next
	// equals prevDoc, which keeps ReindexEngram's DF bookkeeping membership-exact.
	next.Concept, next.CreatedBy, next.Content, next.Tags = prevDoc.Concept, prevDoc.CreatedBy, prevDoc.Content, prevDoc.Tags
	if err := e.fts.ReindexEngram(ws, [16]byte(id), prevDoc, next); err != nil {
		slog.Warn("engine: update digest: fts key-point reindex failed", "id", id.String(), "err", err)
	}
	return nil
}

// matchedKeyPoint returns the key point recall presents as a row's snippet, or
// "" (see fts.Index.BestKeyPoint for the rule).
func (e *Engine) matchedKeyPoint(ws [8]byte, query []string, eng *storage.Engram) string {
	if e.fts == nil || eng == nil || len(eng.KeyPoints) == 0 || len(query) == 0 {
		return ""
	}
	// The same query string the FTS channel searched (activation joins
	// req.Context with a space).
	return e.fts.BestKeyPoint(ws, strings.Join(query, " "), eng.KeyPoints)
}
