package storage

import (
	"context"
	"sync"
	"testing"
)

// P1: two concurrent hard deletes of the same engram must decrement the vault
// counter exactly once. DeleteEngram used to evict the L1 entry only after
// releasing the per-engram stripe lock, so the second deleter's GetEngram
// could hit the stale cached engram, take the full-delete path, and decrement
// a second time.
func TestDeleteEngram_ConcurrentHardDeleteDecrementsOnce(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("double-delete-race")

	const keep = 20
	const rounds = 500
	ids := make([]ULID, 0, keep+rounds)
	for i := 0; i < keep+rounds; i++ {
		id, err := store.WriteEngram(ctx, ws, counterSeedEngram(i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	victims := ids[keep:]
	for _, id := range victims {
		// Populate L1, as a prior recall/read would.
		if _, err := store.GetEngram(ctx, ws, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range victims {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_ = store.DeleteEngram(ctx, ws, id)
			}()
		}
		close(start)
		wg.Wait()
	}
	actual, _ := store.countEngramsForVault(ctx, ws)
	if got := store.GetVaultCount(ctx, ws); got != actual || actual != keep {
		t.Fatalf("counter=%d engram keys=%d, want both %d", got, actual, keep)
	}
}

// Deterministic form of the same defect: a stale L1 entry for an engram that
// is already gone from Pebble (left by a reader that raced the first delete)
// must not make a second DeleteEngram decrement again.
func TestDeleteEngram_StaleL1EntryDoesNotDecrementTwice(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("stale-l1-delete")

	var victim ULID
	for i := 0; i < 3; i++ {
		id, err := store.WriteEngram(ctx, ws, counterSeedEngram(i))
		if err != nil {
			t.Fatal(err)
		}
		victim = id
	}
	stale, err := store.GetEngram(ctx, ws, victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEngram(ctx, ws, victim); err != nil {
		t.Fatal(err)
	}
	// A reader that read Pebble before the commit re-inserts its copy.
	store.cache.Set(ws, victim, stale)
	if err := store.DeleteEngram(ctx, ws, victim); err != nil {
		t.Fatal(err)
	}
	if got := store.GetVaultCount(ctx, ws); got != 2 {
		t.Fatalf("vault counter %d after deleting 1 of 3 engrams twice, want 2", got)
	}
}
