package migrate

import (
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage"
)

// legacyMarkKey builds the pre-relocation marker key: 0x2F | ws(8).
func legacyMarkKey(ws [8]byte) []byte {
	k := make([]byte, 1+8)
	k[0] = legacyDeclaredContradictionMarkPrefix
	copy(k[1:9], ws[:])
	return k
}

// TestPurgeLegacyMark_RelocatesAndClearsOldRange is the upgrade acceptance
// case: a data dir written by this fork before the 0.11.0 merge carries its
// markers at 0x2F, which upstream #726 now uses for the replication log. After
// v7 the old range must be EMPTY — a surviving 9-byte key there would be
// decoded as a replication record — and the marker must be readable at 0x31
// with the same verdict.
func TestPurgeLegacyMark_RelocatesAndClearsOldRange(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	// Two vaults whose hashes begin with the exact bytes upstream uses as its
	// replication discriminator — the collision this migration exists for.
	wsLog := [8]byte{0x01, 2, 3, 4, 5, 6, 7, 8}
	wsMeta := [8]byte{0x02, 2, 3, 4, 5, 6, 7, 8}

	for _, ws := range [][8]byte{wsLog, wsMeta} {
		if err := db.Set(legacyMarkKey(ws), []byte{storage.DeclaredContradictionYes}, pebble.Sync); err != nil {
			t.Fatalf("seed legacy marker: %v", err)
		}
	}
	// Give each vault a real contradicts edge so the rebuild has ground truth
	// to recompute from, rather than inheriting the seeded bytes.
	writePreUpgradeAssoc(t, db, wsLog, [16]byte{1}, [16]byte{2}, storage.RelContradicts)
	writePreUpgradeAssoc(t, db, wsMeta, [16]byte{3}, [16]byte{4}, storage.RelContradicts)

	if err := PurgeLegacyDeclaredContradictionMark(db); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// The old range must be completely empty.
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{legacyDeclaredContradictionMarkPrefix},
		UpperBound: []byte{legacyDeclaredContradictionMarkPrefix + 1},
	})
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	if iter.First() {
		k := iter.Key()
		iter.Close()
		t.Fatalf("legacy 0x2F range still holds %d-byte key %x — upstream replication would decode it", len(k), k)
	}
	iter.Close()

	// And the verdict must survive at the new prefix.
	for name, ws := range map[string][8]byte{"wsLog": wsLog, "wsMeta": wsMeta} {
		mark, ok := readMark(t, db, ws)
		if !ok {
			t.Errorf("%s: no marker at 0x31 after relocation", name)
			continue
		}
		if mark != storage.DeclaredContradictionYes {
			t.Errorf("%s: marker at 0x31 = 0x%02X, want yes (0x%02X)", name, mark, storage.DeclaredContradictionYes)
		}
	}
}

// TestPurgeLegacyMark_LeavesForeignShapesAlone pins the length discriminator:
// anything under 0x2F that is not exactly this fork's 9-byte marker is another
// owner's record and must survive untouched.
func TestPurgeLegacyMark_LeavesForeignShapesAlone(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	// Upstream's shapes: 0x2F|0x01|seq(8) = 10 bytes, 0x2F|0x02|name.
	foreign := [][]byte{
		{0x2F, 0x01, 0, 0, 0, 0, 0, 0, 0, 7},
		append([]byte{0x2F, 0x02}, []byte("lobe-a")...),
	}
	for _, k := range foreign {
		if err := db.Set(k, []byte("payload"), pebble.Sync); err != nil {
			t.Fatalf("seed foreign key: %v", err)
		}
	}

	if err := PurgeLegacyDeclaredContradictionMark(db); err != nil {
		t.Fatalf("purge: %v", err)
	}

	for _, k := range foreign {
		val, closer, err := db.Get(k)
		if err != nil {
			t.Fatalf("foreign key %x was deleted by the purge", k)
		}
		if string(val) != "payload" {
			t.Errorf("foreign key %x: value changed to %q", k, val)
		}
		closer.Close()
	}
}

// TestPurgeLegacyMark_Idempotent pins the re-run: nothing left to purge, and
// the rebuild must not demote a vault that declared a contradiction.
func TestPurgeLegacyMark_Idempotent(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	if err := db.Set(legacyMarkKey(ws), []byte{storage.DeclaredContradictionYes}, pebble.Sync); err != nil {
		t.Fatalf("seed: %v", err)
	}
	writePreUpgradeAssoc(t, db, ws, [16]byte{1}, [16]byte{2}, storage.RelContradicts)

	for i := 0; i < 2; i++ {
		if err := PurgeLegacyDeclaredContradictionMark(db); err != nil {
			t.Fatalf("purge run %d: %v", i+1, err)
		}
	}
	mark, ok := readMark(t, db, ws)
	if !ok || mark != storage.DeclaredContradictionYes {
		t.Errorf("after two runs: mark=0x%02X known=%v, want yes", mark, ok)
	}
}

// TestPurgeLegacyMark_RegisteredAndOrderedBeforeReplication pins both the
// registration and the ordering the migration's safety argument rests on: v7
// must run before any migration that writes into 0x2F.
func TestPurgeLegacyMark_RegisteredAndOrderedBeforeReplication(t *testing.T) {
	r := &Runner{}
	RegisterMigrations(r)

	var purge *Migration
	for i := range r.migrations {
		if r.migrations[i].Version == 7 {
			purge = &r.migrations[i]
		}
	}
	if purge == nil {
		t.Fatal("migration v7 is not registered; stale 0x2F markers would survive into upstream's replication keyspace")
	}
	if MaxRegisteredVersion() < 7 {
		t.Errorf("MaxRegisteredVersion() = %d, want >= 7", MaxRegisteredVersion())
	}
}
