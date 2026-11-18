package migrate

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// staleMarkKey builds the pre-0.12.1 marker key: 0x31 | ws(8).
func staleMarkKey(ws [8]byte) []byte {
	k := make([]byte, 1+8)
	k[0] = staleDeclaredContradictionMarkPrefix
	copy(k[1:9], ws[:])
	return k
}

func openMemDB(t *testing.T) *pebble.DB {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	return db
}

// TestPurge0x31Mark_StaleDeclaredMarkWouldSkipUpstreamRestore is the defect
// this migration exists for, asserted through upstream's own reader: a fork
// marker "declared" (0x01) left at 0x31 reads to upstream's
// GetSupersedesRestoreMark as "restore completed at version 1", so the
// one-time restore of archived supersedes edges would never run for that
// vault. After v11 the watermark must read 0 (not yet run) and the marker must
// be back, with the same verdict, at its new byte.
func TestPurge0x31Mark_StaleDeclaredMarkWouldSkipUpstreamRestore(t *testing.T) {
	db := openMemDB(t)

	wsYes := [8]byte{0x51, 2, 3, 4, 5, 6, 7, 8}
	wsNone := [8]byte{0x52, 2, 3, 4, 5, 6, 7, 8}
	if err := db.Set(staleMarkKey(wsYes), []byte{storage.DeclaredContradictionYes}, pebble.Sync); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Set(staleMarkKey(wsNone), []byte{storage.DeclaredContradictionNone}, pebble.Sync); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Ground truth for the rebuild.
	writePreUpgradeAssoc(t, db, wsYes, [16]byte{1}, [16]byte{2}, storage.RelContradicts)
	writePreUpgradeAssoc(t, db, wsNone, [16]byte{3}, [16]byte{4}, storage.RelSupports)

	if err := PurgeDeclaredContradictionMarkAt0x31(db); err != nil {
		t.Fatalf("purge: %v", err)
	}

	ps := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 16})
	defer ps.Close()
	ctx := context.Background()
	for name, ws := range map[string][8]byte{"wsYes": wsYes, "wsNone": wsNone} {
		v, err := ps.GetSupersedesRestoreMark(ctx, ws)
		if err != nil {
			t.Fatalf("%s: GetSupersedesRestoreMark: %v", name, err)
		}
		if v != 0 {
			t.Errorf("%s: upstream restore watermark reads %d after v11, want 0 — a stale fork marker is still posing as a completed restore", name, v)
		}
	}
	if m, known, err := ps.DeclaredContradictionMark(ctx, wsYes); err != nil || !known || m != storage.DeclaredContradictionYes {
		t.Errorf("wsYes: marker at 0x%02X = (0x%02X, known=%v, err=%v), want yes", prefix.DeclaredContradictionMark, m, known, err)
	}
	if m, known, err := ps.DeclaredContradictionMark(ctx, wsNone); err != nil || !known || m != storage.DeclaredContradictionNone {
		t.Errorf("wsNone: marker at 0x%02X = (0x%02X, known=%v, err=%v), want none", prefix.DeclaredContradictionMark, m, known, err)
	}
}

// TestPurge0x31Mark_ClearsTheRange pins that nothing of the fork's shape is
// left under 0x31, whatever the vault hash.
func TestPurge0x31Mark_ClearsTheRange(t *testing.T) {
	db := openMemDB(t)
	defer db.Close()
	for i := 0; i < 8; i++ {
		ws := [8]byte{byte(i), 0xAA, 0, 0, 0, 0, 0, byte(i)}
		if err := db.Set(staleMarkKey(ws), []byte{byte(i % 2)}, pebble.Sync); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := PurgeDeclaredContradictionMarkAt0x31(db); err != nil {
		t.Fatalf("purge: %v", err)
	}
	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: []byte{0x31}, UpperBound: []byte{0x32}})
	if err != nil {
		t.Fatalf("iter: %v", err)
	}
	defer iter.Close()
	if iter.First() {
		t.Fatalf("0x31 still holds %x after v11", iter.Key())
	}
}

// TestPurge0x31Mark_LeavesForeignShapesAlone pins the shape discriminator:
// anything under 0x31 that is not exactly a 9-byte key with a one-byte 0x00 or
// 0x01 value is left in place.
func TestPurge0x31Mark_LeavesForeignShapesAlone(t *testing.T) {
	db := openMemDB(t)
	defer db.Close()
	ws := [8]byte{7, 7, 7, 7, 7, 7, 7, 7}
	foreign := map[string][2][]byte{
		"future restore version 2": {staleMarkKey(ws), {0x02}},
		"two-byte value":           {staleMarkKey([8]byte{8, 8, 8, 8, 8, 8, 8, 8}), {0x01, 0x00}},
		"empty value":              {staleMarkKey([8]byte{9, 9, 9, 9, 9, 9, 9, 9}), {}},
		"ten-byte key":             {append(staleMarkKey(ws), 0x00), {0x01}},
	}
	for name, kv := range foreign {
		if err := db.Set(kv[0], kv[1], pebble.Sync); err != nil {
			t.Fatalf("%s: seed: %v", name, err)
		}
	}
	if err := PurgeDeclaredContradictionMarkAt0x31(db); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for name, kv := range foreign {
		val, closer, err := db.Get(kv[0])
		if err != nil {
			t.Errorf("%s: deleted by the purge", name)
			continue
		}
		if string(val) != string(kv[1]) {
			t.Errorf("%s: value changed to %x", name, val)
		}
		closer.Close()
	}
}

// TestPurge0x31Mark_Idempotent pins the re-run: nothing left to purge, and the
// rebuild must not demote a vault that declared a contradiction.
func TestPurge0x31Mark_Idempotent(t *testing.T) {
	db := openMemDB(t)
	defer db.Close()
	ws := [8]byte{4, 4, 4, 4, 4, 4, 4, 4}
	if err := db.Set(staleMarkKey(ws), []byte{storage.DeclaredContradictionYes}, pebble.Sync); err != nil {
		t.Fatalf("seed: %v", err)
	}
	writePreUpgradeAssoc(t, db, ws, [16]byte{1}, [16]byte{2}, storage.RelContradicts)
	for i := 0; i < 2; i++ {
		if err := PurgeDeclaredContradictionMarkAt0x31(db); err != nil {
			t.Fatalf("purge run %d: %v", i+1, err)
		}
	}
	mark, ok := readMark(t, db, ws)
	if !ok || mark != storage.DeclaredContradictionYes {
		t.Errorf("after two runs: mark=0x%02X known=%v, want yes", mark, ok)
	}
	if _, closer, err := db.Get(staleMarkKey(ws)); err == nil {
		closer.Close()
		t.Error("stale 0x31 marker survived two runs")
	}
}

// TestPurge0x31Mark_RegisteredAboveEveryAppliedVersion pins the ordering the
// safety argument rests on: v11 is registered, is the highest version (so a
// store stamped at the previously-applied 10 runs it), and the marker no
// longer lives on 0x31.
func TestPurge0x31Mark_RegisteredAboveEveryAppliedVersion(t *testing.T) {
	r := &Runner{}
	RegisterMigrations(r)
	found := false
	for _, m := range r.migrations {
		if m.Version == 11 {
			found = true
		}
	}
	if !found {
		t.Fatal("migration v11 is not registered; fork markers at 0x31 would pose as upstream restore watermarks")
	}
	if got := MaxRegisteredVersion(); got != 11 {
		t.Errorf("MaxRegisteredVersion() = %d, want 11 (a later migration must be added deliberately, above it)", got)
	}
	if prefix.DeclaredContradictionMark == staleDeclaredContradictionMarkPrefix {
		t.Fatal("prefix.DeclaredContradictionMark is still 0x31")
	}
	if k := keys.DeclaredContradictionMarkKey([8]byte{}); k[0] == staleDeclaredContradictionMarkPrefix {
		t.Fatal("keys.DeclaredContradictionMarkKey still builds a 0x31 key")
	}
}

// TestPurge0x31Mark_RunsThroughRunnerFromStamp10 is the upgrade path as the
// deployed binary sees it: a store stamped at 10 (the last fork version
// applied) with fork markers at 0x31 runs exactly v11.
func TestPurge0x31Mark_RunsThroughRunnerFromStamp10(t *testing.T) {
	db := openMemDB(t)
	defer db.Close()
	if err := writeMigrationVersion(db, 10); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	ws := [8]byte{3, 1, 4, 1, 5, 9, 2, 6}
	if err := db.Set(staleMarkKey(ws), []byte{storage.DeclaredContradictionYes}, pebble.Sync); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := NewRunner(db)
	RegisterMigrations(r)
	applied, err := r.Run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if applied != 1 {
		t.Errorf("applied %d migrations from stamp 10, want exactly 1 (v11)", applied)
	}
	if v, _ := readMigrationVersion(db); v != 11 {
		t.Errorf("stamp after run = %d, want 11", v)
	}
	if _, closer, err := db.Get(staleMarkKey(ws)); err == nil {
		closer.Close()
		t.Error("stale 0x31 marker survived the upgrade run")
	}
}
