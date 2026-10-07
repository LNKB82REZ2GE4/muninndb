package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// F2: an in-place FTS rewrite (an enrichment digest's key points, or a
// retag) that lands between a HARD forget's start and its delete must not
// leave postings for the dead engram. Forget(hard) used to de-index an
// unlocked pre-read that predated the rewrite, so the rewrite's new terms
// outlived the engram (26/200 forced races for key points). The seam lands the
// rewrite in exactly that window, so the test is deterministic.
func TestForgetHard_RewriteBeforeDeleteLeavesNoPostings(t *testing.T) {
	cases := []struct {
		name    string
		term    string
		rewrite func(e *Engine, ctx context.Context, vault string, id storage.ULID) error
	}{
		{
			name: "digest key points",
			term: "quokkafjord",
			rewrite: func(e *Engine, ctx context.Context, _ string, id storage.ULID) error {
				return e.UpdateDigestIndexed(ctx, id, "", []string{"quokkafjord stated outright"}, "", "")
			},
		},
		{
			name: "retag",
			term: "wombatgrid",
			rewrite: func(e *Engine, ctx context.Context, vault string, id storage.ULID) error {
				return e.UpdateTags(ctx, vault, id, []string{"wombatgrid"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, store, _ := danglingEnv(t)
			ctx := context.Background()
			const vault = "forget-hard-rewrite"
			resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "plain note", Content: "plain body with nothing special"})
			if err != nil {
				t.Fatal(err)
			}
			id, err := storage.ParseULID(resp.ID)
			if err != nil {
				t.Fatal(err)
			}
			eng.WaitWriteTimeIdle()
			// Warm the read path, as recall would, so the stale record is the
			// one any unlocked pre-read sees.
			if _, err := store.GetEngram(ctx, store.ResolveVaultPrefix(vault), id); err != nil {
				t.Fatal(err)
			}

			forgetHardBeforeDelete = func(got storage.ULID) {
				if got == id {
					if err := tc.rewrite(eng, ctx, vault, id); err != nil {
						t.Errorf("rewrite: %v", err)
					}
				}
			}
			t.Cleanup(func() { forgetHardBeforeDelete = nil })

			if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID, Hard: true}); err != nil {
				t.Fatal(err)
			}
			ws := store.ResolveVaultPrefix(vault)
			hits, err := eng.fts.Search(ctx, ws, tc.term, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range hits {
				if storage.ULID(h.ID) == id {
					t.Fatalf("FTS still returns hard-forgotten engram %s for %q", resp.ID, tc.term)
				}
			}
		})
	}
}
