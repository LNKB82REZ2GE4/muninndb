package migrate

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// writePreUpgradeAssoc writes a raw 0x03 forward-association key directly,
// simulating an edge written by a binary that predates the 0x2F marker: the
// edge exists, no marker was ever written for its vault.
func writePreUpgradeAssoc(t *testing.T, db *pebble.DB, ws [8]byte, src, dst [16]byte, rel storage.RelType) {
	t.Helper()
	// 0x03 value layout (storage/association.go encodeAssocValue):
	// relType(2) | confidence(4) | createdAt(8) | lastActivated(4) | peakWeight(4) | coActivationCount(4)
	const weight float32 = 0.8
	val := make([]byte, 26)
	binary.BigEndian.PutUint16(val[0:2], uint16(rel))
	binary.BigEndian.PutUint32(val[2:6], math.Float32bits(1.0))
	binary.BigEndian.PutUint64(val[6:14], uint64(time.Now().Add(-24*time.Hour).UnixNano()))
	binary.BigEndian.PutUint32(val[18:22], math.Float32bits(weight))
	// Sanity: the migration classifies on exactly this field, so a layout
	// change must break this test rather than silently mark every vault clean.
	if storage.AssocValueRelType(val) != rel {
		t.Fatalf("test fixture encodes relType wrongly: got %v want %v", storage.AssocValueRelType(val), rel)
	}
	if err := db.Set(keys.AssocFwdKey(ws, src, weight, dst), val, pebble.Sync); err != nil {
		t.Fatalf("set 0x03 assoc key: %v", err)
	}
}

func readMark(t *testing.T, db *pebble.DB, ws [8]byte) (byte, bool) {
	t.Helper()
	val, closer, err := db.Get(keys.DeclaredContradictionMarkKey(ws))
	if err != nil {
		return 0, false
	}
	defer closer.Close()
	if len(val) == 0 {
		return 0, false
	}
	return val[0], true
}

// TestDeclaredContradictionMark_Backfill is the pre-upgrade acceptance case:
// vaults whose associations were all written before the 0x2F marker existed
// must be classified correctly in one pass — the vault with a contradicts edge
// marked `yes`, the vault without marked `none` — so the recall gate becomes
// O(1) on a vault far too large to scan per query.
func TestDeclaredContradictionMark_Backfill(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	dirty := [8]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}
	clean := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	empty := [8]byte{0x09, 0x09, 0x09, 0x09, 0x09, 0x09, 0x09, 0x09}

	writePreUpgradeAssoc(t, db, dirty, [16]byte{1}, [16]byte{2}, storage.RelRelatesTo)
	writePreUpgradeAssoc(t, db, dirty, [16]byte{3}, [16]byte{4}, storage.RelContradicts)
	writePreUpgradeAssoc(t, db, dirty, [16]byte{5}, [16]byte{6}, storage.RelSupports)
	writePreUpgradeAssoc(t, db, clean, [16]byte{1}, [16]byte{2}, storage.RelRelatesTo)
	writePreUpgradeAssoc(t, db, clean, [16]byte{3}, [16]byte{4}, storage.RelSupports)

	if _, known := readMark(t, db, dirty); known {
		t.Fatal("precondition: no marker may exist before the migration")
	}

	if err := BackfillDeclaredContradictionMark(db); err != nil {
		t.Fatalf("migration: %v", err)
	}

	if mark, known := readMark(t, db, dirty); !known || mark != storage.DeclaredContradictionYes {
		t.Errorf("vault with a contradicts edge: mark=0x%02X known=%v, want 0x01 true", mark, known)
	}
	if mark, known := readMark(t, db, clean); !known || mark != storage.DeclaredContradictionNone {
		t.Errorf("vault with no contradicts edge: mark=0x%02X known=%v, want 0x00 true", mark, known)
	}
	// A vault with no associations is never seen by the scan and must stay
	// UNKNOWN — claiming `none` for a vault the migration never looked at is
	// exactly the write this migration must not make.
	if _, known := readMark(t, db, empty); known {
		t.Error("a vault with no associations got a marker it was never scanned for")
	}
}

// TestDeclaredContradictionMark_BackfillIsMonotone pins the re-run case: a
// contradicts edge can be archived out of 0x03 (0x25) between passes, and a
// second run seeing only live edges must NOT demote the vault to `none` —
// that would silently turn contradiction honesty off.
func TestDeclaredContradictionMark_BackfillIsMonotone(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{0x77}
	writePreUpgradeAssoc(t, db, ws, [16]byte{1}, [16]byte{2}, storage.RelContradicts)
	if err := BackfillDeclaredContradictionMark(db); err != nil {
		t.Fatal(err)
	}
	if mark, _ := readMark(t, db, ws); mark != storage.DeclaredContradictionYes {
		t.Fatalf("first pass: mark=0x%02X, want 0x01", mark)
	}

	// The contradicts edge goes away; an unrelated edge remains.
	if err := db.DeleteRange([]byte{0x03}, []byte{0x04}, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	writePreUpgradeAssoc(t, db, ws, [16]byte{5}, [16]byte{6}, storage.RelRelatesTo)

	if err := BackfillDeclaredContradictionMark(db); err != nil {
		t.Fatal(err)
	}
	if mark, _ := readMark(t, db, ws); mark != storage.DeclaredContradictionYes {
		t.Errorf("re-run demoted the marker to 0x%02X — a vault that declared a contradiction can now be skipped", mark)
	}
}

// TestDeclaredContradictionMark_RegisteredAsV6 pins the migration into the
// registered set. A migration referenced only from its test is exactly the gap
// that left v3 unapplied on real databases (#611).
func TestDeclaredContradictionMark_RegisteredAsV6(t *testing.T) {
	r := &Runner{}
	RegisterMigrations(r)
	var found *Migration
	for i := range r.migrations {
		if r.migrations[i].Version == 6 {
			found = &r.migrations[i]
		}
	}
	if found == nil {
		t.Fatal("migration v6 is not registered; it would never run on a real database")
	}
	if MaxRegisteredVersion() < 6 {
		t.Errorf("MaxRegisteredVersion() = %d, want >= 6", MaxRegisteredVersion())
	}
}
