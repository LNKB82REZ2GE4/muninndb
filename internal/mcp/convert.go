package mcp

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// contentPreviewLen bounds a default muninn_recall row's content, in
// CHARACTERS (runes), not bytes: a byte bound split multi-byte UTF-8 mid-rune
// and cut non-ASCII content that fit (G11). full_content=true lifts it.
const contentPreviewLen = 500

// previewBoundaryWindow is how far (in characters) previewContent may back
// off from the hard bound to end on a line or word boundary. Past it the cut
// is made at the bound itself, so a preview never shrinks below
// contentPreviewLen-previewBoundaryWindow characters.
const previewBoundaryWindow = 80

// previewContent returns content unchanged when it fits contentPreviewLen
// characters. Otherwise it cuts at the last line break, else the last
// whitespace, within previewBoundaryWindow characters of the bound (else at
// the bound) and appends a VISIBLE marker naming the hidden length and the
// call that recovers it: "…[+5,312 chars, muninn_read <ref>]", where ref is
// the id, plus " vault=<name>" on a multi-vault recall row. The old bare
// "..." left an agent unable to tell a 6 KB page from a 500-char note
// (principle #2). Returns the shown text, whether it was cut, and the full
// length in characters.
func previewContent(content, ref string) (string, bool, int) {
	total := utf8.RuneCountInString(content)
	if total <= contentPreviewLen {
		return content, false, 0
	}
	// Byte offset of the rune at index contentPreviewLen.
	cut, n := len(content), 0
	for i := range content {
		if n == contentPreviewLen {
			cut = i
			break
		}
		n++
	}
	head := content[:cut]
	floor := len(head)
	// Back off at most previewBoundaryWindow runes for the boundary search.
	for k := 0; k < previewBoundaryWindow && floor > 0; k++ {
		_, sz := utf8.DecodeLastRuneInString(head[:floor])
		floor -= sz
	}
	if nl := strings.LastIndexByte(head[floor:], '\n'); nl >= 0 {
		head = head[:floor+nl]
	} else if sp := strings.LastIndexFunc(head[floor:], unicode.IsSpace); sp >= 0 {
		head = head[:floor+sp]
	}
	head = strings.TrimRightFunc(head, unicode.IsSpace)
	if head == "" { // pathological: whitespace-only window at the very start
		head = content[:cut]
	}
	hidden := total - utf8.RuneCountInString(head)
	return head + " …[+" + commaInt(hidden) + " chars, muninn_read " + ref + "]", true, total
}

// commaInt renders a non-negative count with thousands separators (5312 →
// "5,312") for the agent-read truncation marker.
func commaInt(n int) string {
	s := strconv.Itoa(n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// roundScore widens a float32 score to float64 while stripping float32 quantization
// noise (e.g. float64(float32(1.15)) = 1.149999976...). Rounding to 6 decimals keeps
// meaningful precision while serializing clean values. See #502.
func roundScore(f float32) float64 {
	return math.Round(float64(f)*1e6) / 1e6
}

// knownLastAccess converts a wire nanosecond timestamp to a Memory.LastAccess,
// returning nil when the value is not a real access instant.
//
// Both unset shapes land here: erf.ZeroTimeSentinelNanos (the value
// time.Time{}.UnixNano() overflows to, year 1754) and a plain 0 (the Unix
// epoch). Neither is a time a memory was read at, and rendering either one as a
// concrete RFC3339 instant is a plausible-looking wrong answer — the failure
// class principle #2 names as the worst one. Absence is the honest encoding, and
// it is the same answer the staleness annotation already gives for the same
// engram (augmentAnnotations).
func knownLastAccess(ns int64) *time.Time {
	t := time.Unix(0, ns).UTC()
	if storage.IsUnsetTimestamp(t) {
		return nil
	}
	return &t
}

// activationToMemory converts an mbp.ActivationItem to an MCP Memory for recall responses.
// Summary-first by design (#112): Summary carries the enrichment summary, while Content
// carries the real engram content (truncated to a preview). The summary is never copied
// into Content, so the same string is not serialized twice (#502).
func activationToMemory(item *mbp.ActivationItem) Memory {
	return activationToMemoryContent(item, false, false)
}

// activationToMemoryContent is activationToMemory with the G11 opt-in:
// fullContent=true (muninn_recall's full_content argument) returns the whole
// engram content; false returns the bounded, visibly-marked preview.
//
// multiVault is true for a muninn_recall over several vaults. Each row then
// carries its source vault, and the preview marker names it
// ("muninn_read <id> vault=<name>"): muninn_read resolves an id in ONE vault,
// the caller's default unless told otherwise, so a bare-id marker on a row from
// any other vault pointed at "engram not found". A single-vault recall keeps
// the bare-id marker and no vault field: the id resolves in the vault the
// caller just recalled from.
func activationToMemoryContent(item *mbp.ActivationItem, fullContent, multiVault bool) Memory {
	// Content is the real engram content, as a marked preview unless the caller
	// opted into full content. The summary stays in its own field; recall never
	// overwrites Content with Summary.
	shown, truncated, totalChars := item.Content, false, 0
	rowVault := ""
	if multiVault {
		rowVault = item.Vault
	}
	if !fullContent {
		ref := item.ID
		if rowVault != "" {
			ref += " vault=" + rowVault
		}
		shown, truncated, totalChars = previewContent(item.Content, ref)
	}
	// Supersession annotation is ALWAYS surfaced (no annotate flag) so an agent is
	// never handed a stale fact without being told the current one — the "was 8 in
	// May, now 11" narration comes from the payload alone. The annotate=true path
	// augments this same struct with staleness/conflicts/provenance.
	var annotations *MemoryAnnotations
	// SupersededBy/CurrentVersion (asserted) and PossiblySupersededBy/
	// VersionCluster/NewestOfCluster (heuristic, never an authority) are all
	// always-on; annotate=true only augments this struct further below.
	if item.SupersededBy != "" || item.CurrentVersion != "" ||
		item.PossiblySupersededBy != "" || item.VersionCluster != "" || item.NewestOfCluster ||
		item.SubstitutedFor != "" || item.UnresolvedContradiction != nil {
		annotations = &MemoryAnnotations{
			SupersededBy:         item.SupersededBy,
			CurrentVersion:       item.CurrentVersion,
			PossiblySupersededBy: item.PossiblySupersededBy,
			VersionCluster:       item.VersionCluster,
			NewestOfCluster:      item.NewestOfCluster,
			ClusterSize:          item.ClusterSize,
			// COG-28 (#763): asserted substitution provenance. Always-on for
			// the same reason superseded_by is — an agent must never be handed
			// a row admitted by a DIFFERENT memory's match without being told.
			SubstitutedFor:    item.SubstitutedFor,
			ChainTruncated:    item.ChainTruncated,
			HeadNotIndexedYet: item.HeadNotIndexedYet,
			// COG-29 (#764): asserted, unresolved declared contradiction.
			// Always-on — this row's score was demoted because of it, so
			// omitting it would leave the number unexplained.
			UnresolvedContradiction: item.UnresolvedContradiction,
		}
		if b := item.SubstitutionBasis; b != nil {
			annotations.SubstitutionBasis = &SubstitutionBasis{
				AbsoluteScore:      roundScore(b.AbsoluteScore),
				ContentMatch:       roundScore(b.ContentMatch),
				SemanticSimilarity: roundScore(b.SemanticSimilarity),
				FullTextRelevance:  roundScore(b.FullTextRelevance),
			}
		}
	}
	m := Memory{
		Annotations:      annotations,
		ID:               item.ID,
		Vault:            rowVault,
		Concept:          item.Concept,
		Content:          shown,
		ContentTruncated: truncated,
		ContentChars:     totalChars,
		Summary:          item.Summary,
		// G2: the matched key point. Top-level for the same reason as
		// relevance_band: nothing allocation-gated can drop it. It is its own
		// field, so G11's preview cut and marker (inside Content) never touch it.
		MatchedKeyPoint: item.MatchedKeyPoint,
		Score:           roundScore(item.Score),
		VectorScore:     roundScore(item.ScoreComponents.SemanticSimilarity),
		VectorScoreRaw:  roundScore(item.ScoreComponents.SemanticSimilarityRaw),
		EntityBoost:     roundScore(item.ScoreComponents.EntityBoost),
		// #773: the honest, cross-query-comparable quantities. They were
		// computed on every row and mapped onto MBP/REST, and this function —
		// the ONLY path from an activation row to an MCP agent — dropped both.
		AbsoluteScore: roundScore(item.ScoreComponents.AbsoluteScore),
		ContentMatch:  roundScore(item.ScoreComponents.ContentMatch),
		// #773: the band, TOP-LEVEL. Never fold this into the annotations
		// block below — that block is allocated behind a predicate, and a
		// field guarded by it silently vanishes for any row that carries no
		// other annotation (#764).
		RelevanceBand:      item.RelevanceBand,
		RelevanceBandBasis: item.RelevanceBandBasis,
		Confidence:         item.Confidence,
		Why:                item.Why,
		// Map the lifecycle state label the same way the read path does (#502).
		State: storage.LifecycleState(item.State).String(),
		// Type mirrors the vocabulary muninn_remember accepts (storage.ParseMemoryType).
		Type:        storage.MemoryType(item.MemoryType).String(),
		TypeLabel:   item.TypeLabel,
		CreatedAt:   time.Unix(0, item.CreatedAt).UTC(),
		LastAccess:  knownLastAccess(item.LastAccess),
		AccessCount: item.AccessCount,
		Relevance:   item.Relevance,
		SourceType:  item.SourceType,
		Trust:       storage.TrustLevel(item.Trust).String(),
		Tags:        item.Tags,
		Expired:     item.Expired,
	}
	m.Importance, m.ImportanceSource = importanceFields(item.Importance,
		storage.MemoryType(item.MemoryType), storage.TrustLevel(item.Trust))
	// Valid-time annotations: only present when meaningful (backdated
	// valid_from, or a closed window).
	if item.ValidFrom != 0 {
		vf := time.Unix(0, item.ValidFrom).UTC()
		m.ValidFrom = &vf
	}
	if item.ValidUntil != 0 {
		vu := time.Unix(0, item.ValidUntil).UTC()
		m.ValidUntil = &vu
	}
	return m
}

// readResponseToMemory converts a ReadResponse to a Memory for the muninn_read tool.
// Returns the full content without truncation, and maps Summary when present.
// Entities and EntityRelationships are included when populated by the engine.
func readResponseToMemory(r *mbp.ReadResponse) Memory {
	m := Memory{
		ID:      r.ID,
		Concept: r.Concept,
		Content: r.Content, // full content, no truncation
		Summary: r.Summary,
		// G2: the engine's ReadResponse always carried key points (REST
		// returned them); this mapping dropped them from muninn_read.
		KeyPoints:   r.KeyPoints,
		Confidence:  r.Confidence,
		Tags:        r.Tags,
		State:       storage.LifecycleState(r.State).String(),
		Type:        storage.MemoryType(r.MemoryType).String(),
		TypeLabel:   r.TypeLabel,
		CreatedAt:   time.Unix(0, r.CreatedAt).UTC(),
		LastAccess:  knownLastAccess(r.LastAccess),
		AccessCount: r.AccessCount,
		Relevance:   r.Relevance,
		Trust:       storage.TrustLevel(r.Trust).String(),
	}
	m.Importance, m.ImportanceSource = importanceFields(r.Importance,
		storage.MemoryType(r.MemoryType), storage.TrustLevel(r.Trust))
	// muninn_read always echoes the valid-time axis (teaches the two axes:
	// created_at is transaction time, valid_from/valid_until application time).
	if r.ValidFrom != 0 {
		vf := time.Unix(0, r.ValidFrom).UTC()
		m.ValidFrom = &vf
	}
	if r.ValidUntil != 0 {
		vu := time.Unix(0, r.ValidUntil).UTC()
		m.ValidUntil = &vu
	}
	isCurrent := r.IsCurrent
	m.IsCurrent = &isCurrent
	for _, e := range r.Entities {
		m.Entities = append(m.Entities, ReadEntity{Name: e.Name, Type: e.Type})
	}
	for _, rel := range r.EntityRelationships {
		m.EntityRelationships = append(m.EntityRelationships, ReadEntityRel{
			FromEntity: rel.FromEntity,
			ToEntity:   rel.ToEntity,
			RelType:    rel.RelType,
			Weight:     rel.Weight,
		})
	}
	return m
}

// importanceFields resolves the (importance, importance_source) presentation
// pair from a stored importance plus the memory type and trust the effective
// value derives from when unset. "explicit" = the caller asserted the value
// (stored > 0 — writes quantize explicit 0 to 0.01); "derived" = the use-time
// type-table default (never stored). Mirrors storage.EffectiveImportance.
func importanceFields(stored float32, memType storage.MemoryType, trust storage.TrustLevel) (float64, string) {
	eff := roundScore(storage.EffectiveImportance(stored, memType, trust))
	if storage.ImportanceExplicit(stored) {
		return eff, "explicit"
	}
	return eff, "derived"
}

// textContent wraps a string in the MCP tools/call result envelope.
func textContent(s string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": s}},
	}
}
