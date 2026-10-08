package storage

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Restart-aware counter tests. An in-memory count can be right while the
// persisted 0x15 value is wrong: the coalescer and the direct-write paths
// (DeleteEngram, MergeVaultData) both write the key, and a stale queued value
// can win the next flush. Every test here closes the store (which flushes the
// coalescer) and reopens it before asserting, so it checks what a restart
// would actually read.

type counterRestartDir struct {
	t   *testing.T
	dir string
}

func newCounterRestartDir(t *testing.T) *counterRestartDir {
	t.Helper()
	return &counterRestartDir{t: t, dir: t.TempDir()}
}

func (d *counterRestartDir) open() *PebbleStore {
	d.t.Helper()
	db, err := OpenPebble(d.dir, DefaultOptions())
	if err != nil {
		d.t.Fatalf("open pebble: %v", err)
	}
	return NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
}

// countAfterRestart closes s and reports what a fresh process reads.
func (d *counterRestartDir) countAfterRestart(s *PebbleStore, vault string) int64 {
	d.t.Helper()
	if err := s.Close(); err != nil {
		d.t.Fatalf("close: %v", err)
	}
	s2 := d.open()
	defer s2.Close()
	return s2.GetVaultCount(context.Background(), s2.VaultPrefix(vault))
}

func writeN(t *testing.T, s *PebbleStore, ws [8]byte, n, offset int) []ULID {
	t.Helper()
	ids := make([]ULID, n)
	for i := range ids {
		id, err := s.WriteEngram(context.Background(), ws, counterSeedEngram(offset+i))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

// A first-touch delete on a store whose 0x15 key is absent: the in-memory
// result must also be what a restart reads.
func TestVaultCounter_FirstDeleteSurvivesRestart(t *testing.T) {
	d := newCounterRestartDir(t)
	const vault = "counter-delete-restart"
	s := d.open()
	ids := writeN(t, s, s.VaultPrefix(vault), 3, 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = d.open()
	ws := s.VaultPrefix(vault)
	if err := s.db.Delete(keys.VaultCountKey(ws), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEngram(context.Background(), ws, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got := s.GetVaultCount(context.Background(), ws); got != 2 {
		t.Fatalf("in-memory count after delete = %d, want 2", got)
	}
	if got := d.countAfterRestart(s, vault); got != 2 {
		t.Fatalf("count after restart = %d, want 2", got)
	}
}

// A delete straight after writes whose count is still queued on the coalescer:
// the next flush must not write the older, pre-delete count over the delete's.
func TestVaultCounter_DeleteAfterQueuedWritesSurvivesRestart(t *testing.T) {
	d := newCounterRestartDir(t)
	const vault = "counter-delete-queued"
	s := d.open()
	ws := s.VaultPrefix(vault)
	ids := writeN(t, s, ws, 3, 0)
	if err := s.DeleteEngram(context.Background(), ws, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got := d.countAfterRestart(s, vault); got != 2 {
		t.Fatalf("count after restart = %d, want 2", got)
	}
}

// A delete that commits, then has a ClearVault arrive before it decrements,
// must not leave its pre-clear count behind over the cleared vault. The entity
// lock holds the delete inside its post-commit entity cleanup,
// deterministically.
//
// Since vaultClearGate (STO-6a) the clear can no longer LAND inside that
// window: it must wait for the in-flight delete to finish and then clear. The
// test therefore asserts both halves — the clear is held while the delete is
// parked, and once released the count is 0 in memory and after a restart.
func TestVaultCounter_DeleteRacingClearVaultLeavesZero(t *testing.T) {
	d := newCounterRestartDir(t)
	const vault = "counter-delete-clear"
	s := d.open()
	ctx := context.Background()
	ws := s.VaultPrefix(vault)
	ids := writeN(t, s, ws, 5, 0)
	if err := s.UpsertEntityRecord(ctx, ws, EntityRecord{Name: "Alpha", Type: "concept"}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteEntityEngramLink(ctx, ws, ids[0], "Alpha"); err != nil {
		t.Fatal(err)
	}

	mu := s.getEntityLock(ws, "Alpha")
	mu.Lock()
	done := make(chan error, 1)
	go func() { done <- s.DeleteEngram(ctx, ws, ids[0]) }()

	// Wait (bounded) for the delete's commit: its engram key disappears.
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := Get(s.db, keys.EngramKey(ws, [16]byte(ids[0])))
		if err != nil {
			mu.Unlock()
			t.Fatal(err)
		}
		if v == nil { // Get maps not-found to (nil, nil)
			break
		}
		if time.Now().After(deadline) {
			mu.Unlock()
			t.Fatal("delete never committed")
		}
		runtime.Gosched()
	}
	cleared := make(chan error, 1)
	go func() {
		_, err := s.ClearVault(ctx, ws)
		cleared <- err
	}()
	select {
	case err := <-cleared:
		mu.Unlock()
		t.Fatalf("ClearVault completed (err=%v) while a delete of the same vault was mid-flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-cleared; err != nil {
		t.Fatal(err)
	}
	if got := s.GetVaultCount(ctx, ws); got != 0 {
		t.Fatalf("in-memory count after ClearVault = %d, want 0", got)
	}
	if got := d.countAfterRestart(s, vault); got != 0 {
		t.Fatalf("count after restart = %d, want 0", got)
	}
}

// MergeVaultData into a target with no persisted count: Phase 4 used to seed
// the target after the copy, so the scan saw the merged engrams and the merge
// then added them a second time.
func TestVaultCounter_MergeIntoUnseededTargetExact(t *testing.T) {
	d := newCounterRestartDir(t)
	const srcName, dstName = "counter-merge-src", "counter-merge-dst"
	s := d.open()
	ctx := context.Background()
	writeN(t, s, s.VaultPrefix(dstName), 2, 0)
	writeN(t, s, s.VaultPrefix(srcName), 4, 100)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = d.open()
	src, dst := s.VaultPrefix(srcName), s.VaultPrefix(dstName)
	if err := s.db.Delete(keys.VaultCountKey(dst), nil); err != nil {
		t.Fatal(err)
	}
	merged, err := s.MergeVaultData(ctx, src, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if merged != 4 {
		t.Fatalf("merged = %d, want 4", merged)
	}
	if got := s.GetVaultCount(ctx, dst); got != 6 {
		t.Fatalf("in-memory target count = %d, want 6", got)
	}
	if got := d.countAfterRestart(s, dstName); got != 6 {
		t.Fatalf("target count after restart = %d, want 6", got)
	}
}
