package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func markerTestStore(t *testing.T) *PebbleStore {
	t.Helper()
	dir := t.TempDir()
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ps := NewPebbleStore(db, PebbleStoreConfig{})
	t.Cleanup(func() { ps.Close() })
	return ps
}

func writeContradictsEdge(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) {
	t.Helper()
	if err := ps.WriteAssociation(context.Background(), ws, src, dst, &Association{
		TargetID:   dst,
		RelType:    RelContradicts,
		Weight:     0.9,
		Confidence: 1.0,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMergeVaultData_DoesNotLeaveACleanMarkerOverImportedContradiction is the
// RED for the merge back door.
//
// MergeVaultData UNION-copies raw 0x03/0x04 bytes into the target. It never
// calls WriteAssociation, so nothing stages the target's 0x2F marker. A target
// previously proven clean therefore kept its `none` marker after receiving a
// declared `contradicts` edge — and vaultMayHaveContradictions then answers
// "no" forever, skipping COG-29 on a vault that durably contains a declared
// contradiction. Silent, permanent, and reachable from
// POST /api/admin/vaults/{source}/merge-into.
func TestMergeVaultData_DoesNotLeaveACleanMarkerOverImportedContradiction(t *testing.T) {
	ps := markerTestStore(t)
	ctx := context.Background()

	source := ps.ResolveVaultPrefix("merge-source")
	target := ps.ResolveVaultPrefix("merge-target")

	// Source holds a declared contradiction.
	a, b := NewULID(), NewULID()
	writeContradictsEdge(t, ps, source, a, b)

	// Target is proven clean — exactly the state migration v6 leaves behind.
	if err := ps.SetDeclaredContradictionMark(ctx, target, DeclaredContradictionNone); err != nil {
		t.Fatal(err)
	}
	if mark, known, _ := ps.DeclaredContradictionMark(ctx, target); !known || mark != DeclaredContradictionNone {
		t.Fatalf("precondition: target must start proven-clean, got mark=0x%02X known=%v", mark, known)
	}

	if _, err := ps.MergeVaultData(ctx, source, target, nil); err != nil {
		t.Fatal(err)
	}

	// The target now durably contains the contradicts edge.
	assocs, err := ps.GetAssociations(ctx, target, []ULID{a}, 16)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, as := range assocs[a] {
		if as.RelType == RelContradicts {
			found = true
		}
	}
	if !found {
		t.Fatal("precondition: the merge did not copy the contradicts edge into the target")
	}

	mark, known, err := ps.DeclaredContradictionMark(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if known && mark == DeclaredContradictionNone {
		t.Fatal("target still marked CLEAN after a merge imported a declared contradiction — COG-29 is now silently off for this vault")
	}
}

// TestCloneVaultData_DoesNotLeaveACleanMarkerOverCopiedContradiction is the
// same hole through the clone path. A clone target is normally fresh, but the
// copy is a raw byte move either way, so the marker must be reconciled rather
// than assumed absent.
func TestCloneVaultData_DoesNotLeaveACleanMarkerOverCopiedContradiction(t *testing.T) {
	ps := markerTestStore(t)
	ctx := context.Background()

	source := ps.ResolveVaultPrefix("clone-source")
	target := ps.ResolveVaultPrefix("clone-target")

	writeContradictsEdge(t, ps, source, NewULID(), NewULID())
	if err := ps.SetDeclaredContradictionMark(ctx, target, DeclaredContradictionNone); err != nil {
		t.Fatal(err)
	}

	if _, err := ps.CloneVaultData(ctx, source, target, nil); err != nil {
		t.Fatal(err)
	}

	if mark, known, _ := ps.DeclaredContradictionMark(ctx, target); known && mark == DeclaredContradictionNone {
		t.Fatal("clone target still marked CLEAN after copying a declared contradiction")
	}
}

// TestReconcileDeclaredContradictionMark_FailsTowardUnknown pins the rule the
// bulk-copy paths share, including the case that matters most: a source that
// is itself UNKNOWN (never scanned, or scanned past the cap) is not evidence
// that the target is clean, so the target must drop to UNKNOWN rather than
// keep a stale verdict.
func TestReconcileDeclaredContradictionMark_FailsTowardUnknown(t *testing.T) {
	ps := markerTestStore(t)
	ctx := context.Background()

	t.Run("no associations copied leaves the marker alone", func(t *testing.T) {
		ws := ps.ResolveVaultPrefix("recon-noop")
		if err := ps.SetDeclaredContradictionMark(ctx, ws, DeclaredContradictionNone); err != nil {
			t.Fatal(err)
		}
		if err := ps.ReconcileDeclaredContradictionMarkAfterBulkCopy(ctx, ws, false, false); err != nil {
			t.Fatal(err)
		}
		if mark, known, _ := ps.DeclaredContradictionMark(ctx, ws); !known || mark != DeclaredContradictionNone {
			t.Errorf("marker changed on a copy that moved no associations: mark=0x%02X known=%v", mark, known)
		}
	})

	t.Run("declared source promotes the target", func(t *testing.T) {
		ws := ps.ResolveVaultPrefix("recon-yes")
		if err := ps.ReconcileDeclaredContradictionMarkAfterBulkCopy(ctx, ws, true, true); err != nil {
			t.Fatal(err)
		}
		if mark, known, _ := ps.DeclaredContradictionMark(ctx, ws); !known || mark != DeclaredContradictionYes {
			t.Errorf("mark=0x%02X known=%v, want yes", mark, known)
		}
	})

	t.Run("unknown source drops the target to unknown", func(t *testing.T) {
		ws := ps.ResolveVaultPrefix("recon-unknown")
		if err := ps.SetDeclaredContradictionMark(ctx, ws, DeclaredContradictionNone); err != nil {
			t.Fatal(err)
		}
		if err := ps.ReconcileDeclaredContradictionMarkAfterBulkCopy(ctx, ws, true, false); err != nil {
			t.Fatal(err)
		}
		if _, known, _ := ps.DeclaredContradictionMark(ctx, ws); known {
			t.Error("target kept a verdict after edges of unproven provenance were copied in")
		}
	})
}

// TestDeclaredContradictionMark_NoStaleCleanUnderConcurrency is the guard for
// the check-then-set race in SetDeclaredContradictionMark's `none` path.
//
// The interleaving being excluded:
//
//	clean probe: Get -> not yes
//	edge writer:        commits {edge, yes}
//	clean probe:                            Set(none)   <- stale clean, silent
//
// The assertion is an invariant, not a timing trick: at every point after an
// edge writer has committed, the marker must never read `none`. Hammering both
// sides concurrently under -race exercises the lock rather than proving the
// exact interleaving, which is the honest bar for a lock test.
func TestDeclaredContradictionMark_NoStaleCleanUnderConcurrency(t *testing.T) {
	ps := markerTestStore(t)
	ctx := context.Background()

	const rounds = 200
	var wg sync.WaitGroup
	var bad atomic.Int32

	for r := range rounds {
		ws := ps.ResolveVaultPrefix("race-" + string(rune('a'+r%26)) + string(rune('a'+r/26)))
		wg.Add(3)

		// Writer: declares a contradiction (marker set in the same batch).
		go func() {
			defer wg.Done()
			writeContradictsEdge(t, ps, ws, NewULID(), NewULID())
		}()
		// Prober: tries to prove the vault clean.
		go func() {
			defer wg.Done()
			_ = ps.SetDeclaredContradictionMark(ctx, ws, DeclaredContradictionNone)
		}()
		// Reader: the invariant itself.
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				mark, known, err := ps.DeclaredContradictionMark(ctx, ws)
				if err == nil && known && mark == DeclaredContradictionYes {
					// Once yes, it must stay yes for the rest of this round.
					for j := 0; j < 20; j++ {
						m2, k2, e2 := ps.DeclaredContradictionMark(ctx, ws)
						if e2 == nil && k2 && m2 == DeclaredContradictionNone {
							bad.Add(1)
							return
						}
					}
					return
				}
			}
		}()
	}
	wg.Wait()

	if n := bad.Load(); n != 0 {
		t.Fatalf("marker was demoted to CLEAN %d times after a contradiction was declared", n)
	}

	// Final state: every vault that got an edge must read `yes`.
	for r := range rounds {
		ws := ps.ResolveVaultPrefix("race-" + string(rune('a'+r%26)) + string(rune('a'+r/26)))
		mark, known, err := ps.DeclaredContradictionMark(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		if !known || mark != DeclaredContradictionYes {
			t.Fatalf("round %d: final marker is 0x%02X (known=%v), want yes — a clean write clobbered a declared contradiction", r, mark, known)
		}
	}
}
