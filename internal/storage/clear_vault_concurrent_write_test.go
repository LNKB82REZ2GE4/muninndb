package storage

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// P2: ClearVault of one vault while another vault is being written used to
// panic with "sync: WaitGroup is reused before previous Wait has returned".
// The provenance worker tracked in-flight appends in a process-wide
// WaitGroup; ClearVault Waited on it while every concurrent write in ANY
// vault called Add(1) from zero — exactly the reuse the WaitGroup forbids.
func TestClearVault_ConcurrentWritesToOtherVaultDoNotPanic(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	busy := store.VaultPrefix("clear-race-busy")

	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				_, _ = store.WriteEngram(ctx, busy, counterSeedEngram(g*1_000_000+i))
			}
		}(g)
	}
	defer func() {
		stop.Store(true)
		wg.Wait()
	}()

	for r := 0; r < 200; r++ {
		ws := store.VaultPrefix(fmt.Sprintf("clear-race-%d", r))
		if _, err := store.WriteEngram(ctx, ws, counterSeedEngram(r)); err != nil {
			t.Fatal(err)
		}
		var pv any
		func() {
			defer func() { pv = recover() }()
			if _, err := store.ClearVault(ctx, ws); err != nil {
				t.Fatalf("round %d: %v", r, err)
			}
		}()
		if pv != nil {
			t.Fatalf("round %d: ClearVault panicked: %v", r, pv)
		}
	}
}
