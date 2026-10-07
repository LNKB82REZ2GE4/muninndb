package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// The per-vault engram counter (0x15) is seeded lazily, by a scan of the
// vault's 0x01 engram keys, the first time a process touches the vault. Every
// write path increments (or, for a hard delete, decrements) the counter after
// its Pebble commit. If the seed scan runs AFTER that commit, the scan already
// contains the write and the increment counts it a second time: a fresh vault
// whose first write was a batch of 50 reported 100, a first single write
// reported 2. These tests pin the counter to the true engram count for the
// first write on every write path, with no status read beforehand to seed it.

func counterSeedEngram(i int) *Engram {
	return &Engram{Concept: "seed test", Content: fmt.Sprintf("fact number %d", i)}
}

func TestVaultCounter_FirstWriteEngramBatchNotDoubleCounted(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("counter-first-batch")

	const n = 50
	items := make([]EngramBatchItem, n)
	for i := range items {
		items[i] = EngramBatchItem{WSPrefix: ws, Engram: counterSeedEngram(i)}
	}
	_, errs := store.WriteEngramBatch(ctx, items)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	if got := store.GetVaultCount(ctx, ws); got != n {
		t.Fatalf("first-batch vault count = %d, want %d", got, n)
	}
}

func TestVaultCounter_FirstWriteEngramNotDoubleCounted(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("counter-first-single")

	if _, err := store.WriteEngram(ctx, ws, counterSeedEngram(0)); err != nil {
		t.Fatal(err)
	}
	if got := store.GetVaultCount(ctx, ws); got != 1 {
		t.Fatalf("first single-write vault count = %d, want 1", got)
	}
}

func TestVaultCounter_FirstStoreBatchCommitNotDoubleCounted(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("counter-first-storebatch")

	const n = 7
	b := store.NewBatch()
	for i := 0; i < n; i++ {
		if err := b.WriteEngram(ctx, ws, counterSeedEngram(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := store.GetVaultCount(ctx, ws); got != n {
		t.Fatalf("first StoreBatch vault count = %d, want %d", got, n)
	}
}

// Concurrent first writers on a fresh vault: whichever one seeds, every
// writer's commit must land after the seed scan (each loads the counter before
// committing, and sync.Once blocks the rest until the seed finishes), so the
// total is exact. Run under -race.
func TestVaultCounter_ConcurrentFirstWritersExact(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("counter-concurrent-first")

	const writers, per = 8, 10
	var wg sync.WaitGroup
	errCh := make(chan error, writers*per)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			items := make([]EngramBatchItem, per)
			for i := range items {
				items[i] = EngramBatchItem{WSPrefix: ws, Engram: counterSeedEngram(w*per + i)}
			}
			_, errs := store.WriteEngramBatch(ctx, items)
			for _, err := range errs {
				if err != nil {
					errCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := store.GetVaultCount(ctx, ws); got != writers*per {
		t.Fatalf("concurrent first-writers vault count = %d, want %d", got, writers*per)
	}
}

// Control: a status read before the first write seeds the counter from an
// empty vault, so the bug never fired on this ordering. Must stay correct.
func TestVaultCounter_StatusThenBatchCorrect(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("counter-status-first")

	if got := store.GetVaultCount(ctx, ws); got != 0 {
		t.Fatalf("empty vault count = %d, want 0", got)
	}
	const n = 50
	items := make([]EngramBatchItem, n)
	for i := range items {
		items[i] = EngramBatchItem{WSPrefix: ws, Engram: counterSeedEngram(i)}
	}
	_, errs := store.WriteEngramBatch(ctx, items)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	if got := store.GetVaultCount(ctx, ws); got != n {
		t.Fatalf("status-then-batch vault count = %d, want %d", got, n)
	}
}

// The same ordering bug, mirrored: a hard delete that is the first counter
// touch for a vault with no persisted count (e.g. after ClearVault's key
// delete, or a data dir that never persisted one) seeded from a scan that
// already excluded the deleted engram, then decremented again.
func TestVaultCounter_FirstDeleteNotDoubleDecremented(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("counter-first-delete")

	const n = 5
	var ids []ULID
	for i := 0; i < n; i++ {
		id, err := store.WriteEngram(ctx, ws, counterSeedEngram(i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Simulate a process that has never seeded this vault and finds no
	// persisted count: drop the in-memory counter, drop the coalescer's
	// pending value so it cannot re-persist, and delete the persisted key.
	store.vaultCounters.Delete(ws)
	store.counterFlush.Delete(ws)
	if err := store.db.Delete(keys.VaultCountKey(ws), nil); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteEngram(ctx, ws, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got := store.GetVaultCount(ctx, ws); got != n-1 {
		t.Fatalf("first-touch delete vault count = %d, want %d", got, n-1)
	}
}
