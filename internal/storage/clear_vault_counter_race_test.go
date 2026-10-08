package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// P3: writes racing a ClearVault of the SAME vault must leave the counter
// equal to the engram keys that survive. Before vaultClearGate the counter
// drifted in ~all of 200 forced rounds (e.g. counter=25, actual=0): a reader
// re-seeded it from the not-yet-tombstoned keys, and writes committing after
// the tombstone added to an evicted counter.
func TestClearVault_WritesRacingSameVaultClearKeepCounterExact(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	const rounds = 100
	for round := 0; round < rounds; round++ {
		ws := store.VaultPrefix(fmt.Sprintf("clear-same-vault-%d", round))
		for i := 0; i < 5; i++ {
			if _, err := store.WriteEngram(ctx, ws, counterSeedEngram(i)); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				for i := 0; i < 5; i++ {
					_, _ = store.WriteEngram(ctx, ws, counterSeedEngram(100+g*10+i))
					_ = store.GetVaultCount(ctx, ws)
				}
			}(g)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = store.ClearVault(ctx, ws)
		}()
		close(start)
		wg.Wait()
		actual, err := store.countEngramsForVault(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got := store.GetVaultCount(ctx, ws); got != actual {
			t.Fatalf("round %d: counter=%d, engram keys=%d", round, got, actual)
		}
	}
}
