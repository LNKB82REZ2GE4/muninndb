package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestContradictionMarker_WrittenWithTheEdge pins the write path: declaring a
// contradiction must leave a DURABLE 0x2F marker, not just an in-process flag.
// Without it the recall gate has no O(1) way to know, and falls back to the
// capped keyspace scan that a large vault can never complete.
func TestContradictionMarker_WrittenWithTheEdge(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	ws := eng.store.ResolveVaultPrefix("marked")
	contradictionFixture(t, eng, "marked")

	mark, known, err := eng.store.DeclaredContradictionMark(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !known || mark != storage.DeclaredContradictionYes {
		t.Fatalf("after Link(contradicts): mark=0x%02X known=%v, want 0x01 known=true", mark, known)
	}
}

// TestContradictionMarker_GatesWithoutScanning is the point of the whole
// increment: with the durable marker present, the gate must answer from it
// alone — no in-process flag, no capped scan. This is what a 137k-engram vault
// that can never complete the scan relies on.
func TestContradictionMarker_GatesWithoutScanning(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "cleanmark", Concept: "timeout",
		Content: "the request timeout limit is 180ms"}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()
	ws := eng.store.ResolveVaultPrefix("cleanmark")

	// First call runs the probe and must persist its COMPLETE clean verdict.
	if eng.vaultMayHaveContradictions(ctx, ws) {
		t.Fatal("clean vault gated ON")
	}
	mark, known, err := eng.store.DeclaredContradictionMark(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !known || mark != storage.DeclaredContradictionNone {
		t.Fatalf("a completed clean probe did not persist: mark=0x%02X known=%v", mark, known)
	}

	// Simulate a restart: in-process memo gone, marker remains. The gate must
	// still answer without re-probing.
	eng.contradictionProbeClean.Delete(ws)
	eng.contradictionsDeclared.Delete(ws)
	if eng.vaultMayHaveContradictions(ctx, ws) {
		t.Error("gate re-ran the scan instead of reading the durable clean marker")
	}
	if _, memoised := eng.contradictionProbeClean.Load(ws); memoised {
		t.Error("the probe ran again — the O(1) marker path was not taken")
	}

	// And the declared direction, across the same simulated restart.
	contradictionFixture(t, eng, "cleanmark")
	eng.contradictionsDeclared.Delete(ws)
	eng.contradictionProbeClean.Delete(ws)
	if !eng.vaultMayHaveContradictions(ctx, ws) {
		t.Error("gate is off on a vault whose durable marker says a contradiction was declared")
	}
}

// TestContradictionMarker_UnknownFailsTowardHonesty pins the direction of the
// unsafe case. An ABSENT marker means "never proven either way" and must never
// be read as clean: a vault carrying a declared contradiction with no marker
// (the pre-migration state, and the state of any vault whose scan was capped)
// must still get the phase.
func TestContradictionMarker_UnknownFailsTowardHonesty(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	contradictionFixture(t, eng, "unknownmark")
	ws := eng.store.ResolveVaultPrefix("unknownmark")

	// Erase every trace the gate could shortcut on: the durable marker, the
	// in-process flags. The declared 0x03 edge itself stays.
	if err := eng.store.DeleteDeclaredContradictionMark(ctx, ws); err != nil {
		t.Fatal(err)
	}
	eng.contradictionsDeclared.Delete(ws)
	eng.contradictionProbeClean.Delete(ws)

	if !eng.vaultMayHaveContradictions(ctx, ws) {
		t.Fatal("an UNKNOWN marker was read as clean — contradiction honesty is now silently off for this vault")
	}
	resp := recallContradiction(t, eng, "unknownmark", nil)
	if resp.Conflict == nil {
		t.Error("recall stopped honoring a declared contradiction when the marker was absent")
	}
}

// TestContradictionMarker_IsMonotone pins the deliberate residual: the marker
// is never demoted to `none` once a contradiction has been declared. Clearing
// it would require proving no declared edge remains anywhere in the vault —
// the vault-wide scan this marker exists to avoid — and being wrong turns
// honesty off silently. Over-running a read-only phase is the acceptable cost.
func TestContradictionMarker_IsMonotone(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	contradictionFixture(t, eng, "monotone")
	ws := eng.store.ResolveVaultPrefix("monotone")

	if err := eng.store.SetDeclaredContradictionMark(ctx, ws, storage.DeclaredContradictionNone); err != nil {
		t.Fatal(err)
	}
	mark, known, err := eng.store.DeclaredContradictionMark(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !known || mark != storage.DeclaredContradictionYes {
		t.Fatalf("marker was demoted to 0x%02X — a declared contradiction can now be skipped silently", mark)
	}
}
