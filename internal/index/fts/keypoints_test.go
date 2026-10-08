package fts

import (
	"context"
	"math"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// Key points (G2) are the enricher's 3-7 self-contained statements about an
// engram. They are indexed in their own field so a query that matches ONE key
// point finds the engram even when the raw content words it differently or
// buries the statement deep in a long body.

func kpScore(t *testing.T, idx *Index, ws [8]byte, q string, id [16]byte) float64 {
	t.Helper()
	res, err := idx.Search(context.Background(), ws, q, 50)
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	for _, r := range res {
		if r.ID == id {
			return r.Score
		}
	}
	return 0
}

// A term that appears ONLY in a key point must make the engram findable.
func TestKeyPoints_KeyPointOnlyTermIsSearchable(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("kp-searchable")

	target := [16]byte{1}
	doc := Document{
		Concept:   "session turn",
		Content:   "we talked through the rollout and agreed on the next steps",
		KeyPoints: []string{"The quokka cache is flushed nightly at 03:30.", "Rollout waits for the drill."},
	}
	if err := idx.IndexDocument(ws, target, doc); err != nil {
		t.Fatalf("IndexDocument: %v", err)
	}
	if err := idx.IndexEngram(ws, [16]byte{2}, "other", "", "an unrelated note about rollout timing", nil); err != nil {
		t.Fatalf("IndexEngram: %v", err)
	}

	if s := kpScore(t, idx, ws, "quokka cache flushed", target); s <= 0 {
		t.Fatalf("query matching only a key point: target score %.4f, want > 0", s)
	}
}

// Key points must never ADD to a term the content already covers well: the
// per-(term, doc) coverage is max(content side, key-point side), and a
// key-point mention is credited like ONE mention in an average-length document
// of this vault. So an engram whose content is no longer than the vault average
// scores exactly as it would unenriched, and no enriched engram can outscore a
// plain average-length engram that states the same words. This is the "help a
// matching engram, do not inflate every enriched engram" property.
func TestKeyPoints_DoNotInflateTermsContentAlreadyCovers(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("kp-no-inflate")

	const content = "the ingestion queue retries failed batches with exponential backoff"
	restated := []string{"The ingestion queue retries failed batches with exponential backoff."}
	plain, enriched := [16]byte{1}, [16]byte{2}
	if err := idx.IndexDocument(ws, plain, Document{Concept: "queue note", Content: content}); err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexDocument(ws, enriched, Document{Concept: "queue note", Content: content, KeyPoints: restated}); err != nil {
		t.Fatal(err)
	}
	// Longer background documents put the two targets BELOW the vault average.
	for i := range 6 {
		if err := idx.IndexEngram(ws, [16]byte{byte(10 + i)}, "filler", "",
			"unrelated filler text about gardens and hedges and the long walk around the orchard every morning", nil); err != nil {
			t.Fatal(err)
		}
	}

	q := "ingestion queue exponential backoff"
	sp, se := kpScore(t, idx, ws, q, plain), kpScore(t, idx, ws, q, enriched)
	if sp <= 0 {
		t.Fatalf("plain engram not found (score %.4f)", sp)
	}
	if math.Abs(sp-se) > 1e-9 {
		t.Fatalf("key points restating shorter-than-average content changed the score: plain %.6f, enriched %.6f (want equal)", sp, se)
	}

	// A long enriched engram restating the same words can be lifted, but never
	// above a plain engram that states them at average length.
	long := content
	for range 20 {
		long += " plus a long tail of narrative about unrelated work items"
	}
	longEnriched := [16]byte{3}
	if err := idx.IndexDocument(ws, longEnriched, Document{Concept: "queue note", Content: long, KeyPoints: restated}); err != nil {
		t.Fatal(err)
	}
	if sl := kpScore(t, idx, ws, q, longEnriched); sl > sp+1e-9 {
		t.Fatalf("long enriched engram outscored the plain one on restated words: %.6f > %.6f", sl, sp)
	}
}

// A long body that mentions a fact once is length-penalized by BM25; the same
// fact as a short key point is not. The key-point side lifts the engram.
func TestKeyPoints_LiftLongBodyMatch(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("kp-lift")

	long := "pagination detail "
	for range 60 {
		long += "assorted narrative words describing many unrelated steps of the session "
	}
	long += "and the watchdog threshold ended at nine seconds"
	without, with := [16]byte{1}, [16]byte{2}
	if err := idx.IndexDocument(ws, without, Document{Concept: "turn", Content: long}); err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexDocument(ws, with, Document{Concept: "turn", Content: long,
		KeyPoints: []string{"The watchdog threshold was set to nine seconds."}}); err != nil {
		t.Fatal(err)
	}
	// Ordinary short memories make the two long bodies longer than this
	// vault's average, which is where BM25 length normalisation bites.
	for i := range 8 {
		if err := idx.IndexEngram(ws, [16]byte{byte(20 + i)}, "note", "", "a short ordinary memory", nil); err != nil {
			t.Fatal(err)
		}
	}
	q := "watchdog threshold nine seconds"
	sw, sk := kpScore(t, idx, ws, q, without), kpScore(t, idx, ws, q, with)
	if !(sk > sw) {
		t.Fatalf("key point did not lift a long-body match: without %.4f, with %.4f", sw, sk)
	}
}

// Key points must not change the BM25 length normaliser of the content: a
// query on content words the key points do not mention scores identically.
func TestKeyPoints_DoNotChangeContentLengthNormalisation(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("kp-doclen")

	const content = "the gardener pruned the roses and replanted tulips by the gate"
	a, b := [16]byte{1}, [16]byte{2}
	if err := idx.IndexDocument(ws, a, Document{Concept: "garden", Content: content}); err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexDocument(ws, b, Document{Concept: "garden", Content: content,
		KeyPoints: []string{"Seven entirely different statements about quarterly invoices.", "Budget approvals moved to Fridays."}}); err != nil {
		t.Fatal(err)
	}
	q := "gardener pruned roses"
	sa, sb := kpScore(t, idx, ws, q, a), kpScore(t, idx, ws, q, b)
	if sa <= 0 || math.Abs(sa-sb) > 1e-9 {
		t.Fatalf("key points changed a content-only match: %.6f vs %.6f (want equal, > 0)", sa, sb)
	}
}

// DeleteDocument must drop key-point postings too, or a forgotten engram stays
// reachable through its key points.
func TestKeyPoints_DeleteDocumentRemovesKeyPointPostings(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("kp-delete")

	id := [16]byte{7}
	doc := Document{Concept: "c", Content: "plain words", KeyPoints: []string{"The narwhal ledger closes monthly."}}
	if err := idx.IndexDocument(ws, id, doc); err != nil {
		t.Fatal(err)
	}
	if kpScore(t, idx, ws, "narwhal ledger", id) <= 0 {
		t.Fatal("precondition: key point not searchable")
	}
	if err := idx.DeleteDocument(ws, id, doc); err != nil {
		t.Fatal(err)
	}
	if s := kpScore(t, idx, ws, "narwhal ledger", id); s != 0 {
		t.Fatalf("deleted engram still reachable through a key point (score %.4f)", s)
	}
}

// ReindexEngram replacing the key points (re-enrichment) must drop the old
// key-point postings and keep the DF bookkeeping membership-based.
func TestKeyPoints_ReindexReplacesKeyPoints(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("kp-reindex")

	id := [16]byte{3}
	prev := Document{Concept: "c", Content: "plain words here"}
	if err := idx.IndexDocument(ws, id, prev); err != nil {
		t.Fatal(err)
	}
	next := prev
	next.KeyPoints = []string{"The axolotl budget doubled."}
	if err := idx.ReindexEngram(ws, id, prev, next); err != nil {
		t.Fatal(err)
	}
	if kpScore(t, idx, ws, "axolotl budget", id) <= 0 {
		t.Fatal("key point added by reindex is not searchable")
	}
	if df, missing := dfOf(t, idx, ws, "axolotl"); missing || df != 1 {
		t.Fatalf("df(axolotl) = %d (missing=%v), want 1", df, missing)
	}
	third := prev
	third.KeyPoints = []string{"The pelican roster changed."}
	if err := idx.ReindexEngram(ws, id, next, third); err != nil {
		t.Fatal(err)
	}
	if s := kpScore(t, idx, ws, "axolotl budget", id); s != 0 {
		t.Fatalf("replaced key point still matches (score %.4f)", s)
	}
	if df, _ := dfOf(t, idx, ws, "axolotl"); df != 0 {
		t.Fatalf("df(axolotl) = %d after its key point left, want 0", df)
	}
	if kpScore(t, idx, ws, "pelican roster", id) <= 0 {
		t.Fatal("new key point not searchable")
	}
}

func TestBestKeyPoint_PicksTheMatchedStatement(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("kp-best")

	kps := []string{
		"The deploy script now pins the toolchain version.",
		"The heron migration is scheduled for Thursday night.",
		"Backups are verified weekly.",
	}
	if err := idx.IndexDocument(ws, [16]byte{1}, Document{Concept: "c", Content: "notes", KeyPoints: kps}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		_ = idx.IndexEngram(ws, [16]byte{byte(10 + i)}, "x", "", "deploy notes and other words", nil)
	}
	if got := idx.BestKeyPoint(ws, "when is the heron migration scheduled", kps); got != kps[1] {
		t.Fatalf("BestKeyPoint = %q, want %q", got, kps[1])
	}
	// Covering less than half of the query's IDF mass is not a match: a key
	// point that shares one common word must not be presented as the snippet.
	if got := idx.BestKeyPoint(ws, "quarterly invoice reconciliation deploy", kps); got != "" {
		t.Fatalf("weak overlap returned a snippet %q, want none", got)
	}
	if got := idx.BestKeyPoint(ws, "", kps); got != "" {
		t.Fatalf("empty query returned %q", got)
	}
	if got := idx.BestKeyPoint(ws, "heron migration", nil); got != "" {
		t.Fatalf("no key points returned %q", got)
	}
}
