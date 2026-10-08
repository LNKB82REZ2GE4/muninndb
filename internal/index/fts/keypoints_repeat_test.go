package fts

import (
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// Enrichers repeat the topic noun in every key point. Key-point TF is presence,
// so the repeats cannot lift an enriched engram above an identical plain one.
func TestKeyPoints_RepeatedTermsDoNotInflate(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("kp-repeat")

	const content = "the ingestion queue retries failed batches with exponential backoff"
	kps := []string{"The ingestion queue retries failed batches.", "The queue uses exponential backoff.", "Backoff applies per queue batch."}
	plain, enriched := [16]byte{1}, [16]byte{2}
	if err := idx.IndexDocument(ws, plain, Document{Concept: "queue note", Content: content}); err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexDocument(ws, enriched, Document{Concept: "queue note", Content: content, KeyPoints: kps}); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		if err := idx.IndexEngram(ws, [16]byte{byte(10 + i)}, "filler", "",
			"unrelated filler text about gardens and hedges and the long walk around the orchard every morning", nil); err != nil {
			t.Fatal(err)
		}
	}
	q := "ingestion queue exponential backoff"
	sp, se := kpScore(t, idx, ws, q, plain), kpScore(t, idx, ws, q, enriched)
	if sp <= 0 || se > sp+1e-9 {
		t.Fatalf("repeated key-point terms inflated the score: plain %.6f, enriched %.6f", sp, se)
	}
}
