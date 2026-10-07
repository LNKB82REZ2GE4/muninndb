package plugin

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// The in-process enrich pipeline persists through PluginStore.UpdateDigest; an
// indexed adapter must route that through the digest writer (the engine's
// UpdateDigestIndexed) so key points it writes reach the FTS index (G2).
func TestIndexedStoreAdapter_UpdateDigestUsesDigestWriter(t *testing.T) {
	var gotID storage.ULID
	var gotKP []string
	calls := 0
	w := func(_ context.Context, id storage.ULID, summary string, kps []string, mt, tl string) error {
		calls++
		gotID, gotKP = id, kps
		return nil
	}
	a := NewIndexedStoreAdapter(nil, nil, w)
	id := ULID(storage.NewULID())
	if err := a.UpdateDigest(context.Background(), id, &EnrichmentResult{Summary: "s", KeyPoints: []string{"k1", "k2"}}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || gotID != storage.ULID(id) || len(gotKP) != 2 {
		t.Fatalf("digest writer calls=%d id=%v kps=%q", calls, gotID, gotKP)
	}
}
