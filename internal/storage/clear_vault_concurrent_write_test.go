package storage

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// ClearVault's provenance drain is scoped to the vault being cleared: an
// append in flight for a DIFFERENT vault must not hold it up (a busy vault
// would otherwise starve every clear for as long as it keeps writing).
func TestProvenanceWorker_DrainVaultIgnoresOtherVaults(t *testing.T) {
	store := openTestStore(t)
	w := store.provWork
	other := store.VaultPrefix("drain-scope-other")
	target := store.VaultPrefix("drain-scope-target")

	w.acquire(other) // an append for another vault, still in flight
	done := make(chan struct{})
	go func() {
		w.DrainVault(target)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainVault(target) blocked on an in-flight append for another vault")
	}

	w.acquire(target)
	drained := make(chan struct{})
	go func() {
		w.DrainVault(target)
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("DrainVault(target) returned while an append for target was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	w.release(target)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainVault(target) did not return after the in-flight append finished")
	}
	w.release(other)
}
