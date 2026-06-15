package hnsw

import (
	"context"
	"math/rand"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// buildPersistedGraph stores n random vectors and inserts them into a fresh
// Index, then blocks until async persistence completes. It mirrors the
// production write path (StoreVector persists the 0xFF vector slot; Insert
// persists the per-layer neighbor lists).
func buildPersistedGraph(t *testing.T, db *pebble.DB, ws [8]byte, n, dim int, seed int64) (*Index, [][16]byte, [][]float32) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	idx := New(db, ws)
	ids := make([][16]byte, n)
	vecs := make([][]float32, n)
	for i := 0; i < n; i++ {
		var id [16]byte
		rng.Read(id[:])
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()*2 - 1
		}
		ids[i], vecs[i] = id, v
		if err := idx.StoreVector(id, v); err != nil {
			t.Fatalf("StoreVector: %v", err)
		}
		idx.Insert(id, v)
	}
	idx.Close() // wait for async persistNode goroutines
	return idx, ids, vecs
}

// TestLoadFromPebble_DisconnectedEntryPoint_Rebuilds reproduces the production
// failure (an entry point whose persisted neighbor list is only itself) and
// asserts the load path detects the disconnection and rebuilds the graph from
// the still-intact vectors. Without the rebuild, bfsReachable from the
// self-looped entry point is 1 and this test fails.
func TestLoadFromPebble_DisconnectedEntryPoint_Rebuilds(t *testing.T) {
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var ws [8]byte
	copy(ws[:], []byte("REBUILD1"))
	const n, dim = 60, 16

	_, _, vecs := buildPersistedGraph(t, db, ws, n, dim, 11)

	// Discover the node the restore path deterministically selects as entry
	// point (the highest persisted layer), and confirm the freshly built graph
	// is navigable — so the only thing that can make the reload navigable again
	// is the rebuild.
	probe := New(db, ws)
	if err := probe.LoadFromPebble(); err != nil {
		t.Fatalf("probe LoadFromPebble: %v", err)
	}
	entry := probe.entryPoint
	if got := bfsReachable(probe.nodes, entry); got < n*9/10 {
		t.Fatalf("precondition: freshly built graph not navigable (reachable=%d/%d)", got, n)
	}

	// Corrupt the persisted entry point's layer-0 neighbor list to [self] — the
	// degenerate apex observed in production. The 0xFF vector slots are left
	// intact, so the vectors remain the reliable source of truth for a rebuild.
	if err := db.Set(keys.HNSWNodeKey(ws, entry, 0), encodeNeighbors([][16]byte{entry}), pebble.Sync); err != nil {
		t.Fatalf("corrupt entry point: %v", err)
	}

	idx2 := New(db, ws)
	if err := idx2.LoadFromPebble(); err != nil {
		t.Fatalf("LoadFromPebble: %v", err)
	}

	if got := bfsReachable(idx2.nodes, idx2.entryPoint); got < n*9/10 {
		t.Fatalf("post-load reachable=%d/%d — disconnected graph was not rebuilt", got, n)
	}

	res, err := idx2.Search(context.Background(), vecs[0], 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 || res[0].Score <= 0 {
		t.Fatalf("search on rebuilt index returned no usable result: %+v", res)
	}
}

// TestLoadFromPebble_HealthyGraph_LoadsNavigable guards the common path: a
// healthy persisted graph must load already-navigable, so the rebuild branch is
// a no-op and load remains correct.
func TestLoadFromPebble_HealthyGraph_LoadsNavigable(t *testing.T) {
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var ws [8]byte
	copy(ws[:], []byte("HEALTHY1"))
	const n, dim = 60, 16

	buildPersistedGraph(t, db, ws, n, dim, 23)

	idx2 := New(db, ws)
	if err := idx2.LoadFromPebble(); err != nil {
		t.Fatalf("LoadFromPebble: %v", err)
	}
	if got := bfsReachable(idx2.nodes, idx2.entryPoint); got < n*9/10 {
		t.Fatalf("healthy graph loaded with reachable=%d/%d", got, n)
	}
}
