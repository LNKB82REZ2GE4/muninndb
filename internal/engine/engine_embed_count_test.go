package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestCountEmbeddableTotal_MatchesCountEmbeddedFilter pins the fix for a
// review-caught asymmetry: Stat's EngramCount (e.engramCount) only decrements
// on a HARD delete, never on soft delete or archive, while CountEmbedded
// (via storage.CountEngramsWithFlag) excludes both. Comparing EmbeddedCount
// against Stat's EngramCount would let EmbeddedCount sit permanently below
// the total for a vault holding embedded-then-soft-deleted engrams — the
// embed-status API's `indexing` bool would report true forever with nothing
// left to embed. CountEmbeddableTotal must apply the identical live-state
// filter CountEmbedded does, so the two stay comparable.
func TestCountEmbeddableTotal_MatchesCountEmbeddedFilter(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "embeddable-total-vault"
	const DigestEmbed uint8 = 0x02

	idStrings := make([]string, 3)
	for i := range idStrings {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vaultName,
			Concept: "concept",
			Content: fmt.Sprintf("content for embeddable-total test #%d", i),
		})
		if err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
		idStrings[i] = resp.ID
	}

	for _, s := range idStrings {
		id, err := storage.ParseULID(s)
		if err != nil {
			t.Fatalf("parse id: %v", err)
		}
		if err := eng.store.SetDigestFlag(ctx, id, DigestEmbed); err != nil {
			t.Fatalf("SetDigestFlag: %v", err)
		}
	}

	// Before any deletion: both counts must be equal (every live engram is embedded).
	embedded := eng.CountEmbedded(ctx)
	total := eng.CountEmbeddableTotal(ctx)
	if embedded != total {
		t.Fatalf("before soft delete: CountEmbedded=%d, CountEmbeddableTotal=%d, want equal", embedded, total)
	}

	// Soft-delete one embedded engram (Forget without Hard).
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vaultName, ID: idStrings[0]}); err != nil {
		t.Fatalf("Forget (soft): %v", err)
	}

	embedded = eng.CountEmbedded(ctx)
	total = eng.CountEmbeddableTotal(ctx)
	if embedded != total {
		t.Errorf("after soft delete: CountEmbedded=%d, CountEmbeddableTotal=%d, want equal (both must exclude the soft-deleted engram)", embedded, total)
	}
}
