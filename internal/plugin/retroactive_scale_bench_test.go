package plugin

import (
	"context"
	"fmt"
	"testing"
	"time"

	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

// noopScaleEmbedPlugin is a minimal EmbedPlugin for scale measurements: it
// does no real inference, just returns zero vectors, so the measurement
// isolates RetroactiveProcessor/PluginStore overhead from any provider cost.
type noopScaleEmbedPlugin struct {
	mockPlugin
	dim      int
	maxBatch int
}

func (p *noopScaleEmbedPlugin) Embed(ctx context.Context, texts []string) ([]float32, error) {
	return make([]float32, len(texts)*p.dim), nil
}
func (p *noopScaleEmbedPlugin) Dimension() int    { return p.dim }
func (p *noopScaleEmbedPlugin) MaxBatchSize() int { return p.maxBatch }

// TestRetroactiveProcessor_EmbedPass_ManyVaults_ScalesWithBacklogNotCorpus
// characterizes (and loosely bounds) the production stall this fix targets
// (COG-embed-vault-scan): one processBatch() pass, through the REAL
// pluginStoreAdapter/PebbleStore/HNSW stack (not mocks), against a corpus
// spread across many vaults with most engrams already embedded and a small
// pending backlog in the LAST vault (so any per-candidate cost proportional
// to total corpus size — not just backlog size — shows up directly in wall
// time).
//
// This is a loose smoke bound, not a tight perf assertion (CI hardware
// varies, and CountWithoutFlag/ScanWithoutFlag's own full-keyspace scan is a
// separate, NOT-fixed-here O(corpus) cost paid once per pass regardless —
// see the doc comment on those methods for why). Measured on this machine:
// pre-fix (FindVaultPrefix re-derived per candidate call) ~17s for this
// corpus; post-fix ~3.4s, dominated by that separate per-pass scan cost, not
// by anything proportional to candidate count. The budget below is set well
// above the post-fix number so normal hardware variance doesn't flake, while
// still catching a regression back to the pre-fix O(candidates × corpus)
// shape. To see the actual pre-fix cost yourself, temporarily revert
// store_adapter.go's ws parameters (reintroducing the internal
// FindVaultPrefix calls) and re-run with -v.
func TestRetroactiveProcessor_EmbedPass_ManyVaults_ScalesWithBacklogNotCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale characterization in -short mode")
	}

	const numVaults = 20
	const engramsPerVault = 1000 // 20 * 1000 = 20,000 total engrams
	const backlogSize = 50       // pending (unembedded) engrams, in the LAST vault

	db, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	defer store.Close()

	ctx := context.Background()
	const DigestEmbed uint8 = 0x02

	var backlogIDs []storage.ULID
	for v := 0; v < numVaults; v++ {
		ws := store.VaultPrefix(fmt.Sprintf("bench-vault-%d", v))
		for i := 0; i < engramsPerVault; i++ {
			id, err := store.WriteEngram(ctx, ws, &storage.Engram{
				Concept: "concept", Content: fmt.Sprintf("content %d-%d", v, i),
			})
			if err != nil {
				t.Fatalf("WriteEngram: %v", err)
			}
			if v == numVaults-1 && i < backlogSize {
				backlogIDs = append(backlogIDs, id)
				continue
			}
			if err := store.SetDigestFlag(ctx, id, DigestEmbed); err != nil {
				t.Fatalf("SetDigestFlag: %v", err)
			}
		}
	}
	t.Logf("seeded %d engrams across %d vaults, %d pending", numVaults*engramsPerVault, numVaults, len(backlogIDs))

	reg := hnswpkg.NewRegistry(db)
	pStore := NewStoreAdapter(store, reg)
	embedPlugin := &noopScaleEmbedPlugin{
		mockPlugin: mockPlugin{name: "bench-embed", tier: TierEmbed},
		dim:        384,
		maxBatch:   64,
	}
	rp := NewRetroactiveProcessor(pStore, embedPlugin, DigestEmbed)

	start := time.Now()
	if ok := rp.processBatch(ctx); !ok {
		t.Fatal("processBatch returned false")
	}
	elapsed := time.Since(start)
	t.Logf("processBatch pass over %d-engram corpus (%d pending): %v", numVaults*engramsPerVault, len(backlogIDs), elapsed)

	if rp.Stats().Processed != int64(len(backlogIDs)) {
		t.Fatalf("processed = %d, want %d (the backlog)", rp.Stats().Processed, len(backlogIDs))
	}

	// Generous smoke bound — see doc comment.
	const budget = 3 * time.Second
	if elapsed > budget {
		t.Errorf("pass took %v, want under %v for a %d-engram corpus with only %d pending — looks like a per-candidate cost is scaling with total corpus size again",
			elapsed, budget, numVaults*engramsPerVault, len(backlogIDs))
	}
}
