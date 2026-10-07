package migrate

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func putEngramKeys(t *testing.T, db *pebble.DB, ws [8]byte, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var id [16]byte
		binary.BigEndian.PutUint64(id[8:], uint64(i+1))
		if err := db.Set(keys.EngramKey(ws, id), []byte{0}, pebble.NoSync); err != nil {
			t.Fatal(err)
		}
	}
}

func putCount(t *testing.T, db *pebble.DB, ws [8]byte, v int64) {
	t.Helper()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	if err := db.Set(keys.VaultCountKey(ws), buf[:], pebble.NoSync); err != nil {
		t.Fatal(err)
	}
}

func readCount(t *testing.T, db *pebble.DB, ws [8]byte) (int64, bool) {
	t.Helper()
	v, closer, err := db.Get(keys.VaultCountKey(ws))
	if err == pebble.ErrNotFound {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if len(v) != 8 {
		t.Fatalf("count value len = %d", len(v))
	}
	return int64(binary.BigEndian.Uint64(v)), true
}

// TestRecountVaultCounters_CorrectsDrift builds a data dir whose persisted
// counters are deliberately wrong in every shape the migration must handle and
// checks each is rewritten to the vault's real engram-key count, while the
// other shapes are left exactly as they were.
func TestRecountVaultCounters_CorrectsDrift(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}

	wsOver := [8]byte{1, 1, 1, 1, 1, 1, 1, 1}      // the G9 shape: first batch of 50 counted twice
	wsUnder := [8]byte{2, 2, 2, 2, 2, 2, 2, 2}     // below the truth (mirror delete bug)
	wsRight := [8]byte{3, 3, 3, 3, 3, 3, 3, 3}     // already correct: must not be rewritten
	wsEmpty := [8]byte{4, 4, 4, 4, 4, 4, 4, 4}     // counter with no engrams left
	wsMalformed := [8]byte{5, 5, 5, 5, 5, 5, 5, 5} // truncated value
	wsNoKey := [8]byte{6, 6, 6, 6, 6, 6, 6, 6}     // engrams but no counter: lazy seed's job
	// Adjacent ws bytes so a scan boundary error between vaults would show.
	wsNeighbour := [8]byte{1, 1, 1, 1, 1, 1, 1, 2}

	putEngramKeys(t, db, wsOver, 904)
	putCount(t, db, wsOver, 954)
	putEngramKeys(t, db, wsNeighbour, 3)
	putCount(t, db, wsNeighbour, 3)
	putEngramKeys(t, db, wsUnder, 10)
	putCount(t, db, wsUnder, 7)
	putEngramKeys(t, db, wsRight, 12)
	putCount(t, db, wsRight, 12)
	putCount(t, db, wsEmpty, 50)
	putEngramKeys(t, db, wsMalformed, 4)
	if err := db.Set(keys.VaultCountKey(wsMalformed), []byte{1, 2, 3}, pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	putEngramKeys(t, db, wsNoKey, 5)

	if err := RecountVaultCounters(db); err != nil {
		t.Fatalf("migration: %v", err)
	}

	for _, c := range []struct {
		name string
		ws   [8]byte
		want int64
	}{
		{"overcount", wsOver, 904},
		{"neighbour", wsNeighbour, 3},
		{"undercount", wsUnder, 10},
		{"already-right", wsRight, 12},
		{"empty", wsEmpty, 0},
		{"malformed", wsMalformed, 4},
	} {
		got, ok := readCount(t, db, c.ws)
		if !ok || got != c.want {
			t.Errorf("%s: count = %d (present=%v), want %d", c.name, got, ok, c.want)
		}
	}
	if _, ok := readCount(t, db, wsNoKey); ok {
		t.Errorf("no-key vault: migration created a counter; that is the lazy seed's job")
	}

	// Idempotent: a second run changes nothing.
	if err := RecountVaultCounters(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got, _ := readCount(t, db, wsOver); got != 904 {
		t.Errorf("second run moved overcount vault to %d", got)
	}

	// End to end: a store opened on the migrated dir reports the truth, and the
	// no-key vault is seeded correctly on first touch.
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 10})
	defer store.Close()
	ctx := context.Background()
	if got := store.GetVaultCount(ctx, wsOver); got != 904 {
		t.Errorf("store GetVaultCount(overcount) = %d, want 904", got)
	}
	if got := store.GetVaultCount(ctx, wsNoKey); got != 5 {
		t.Errorf("store GetVaultCount(no-key) = %d, want 5", got)
	}
}

// TestRecountVaultCounters_RegisteredAboveForkHighWaterMark pins the
// numbering: live fork data dirs are stamped at 9, so the recount must be
// registered above it or the runner would skip it silently.
func TestRecountVaultCounters_RegisteredAboveForkHighWaterMark(t *testing.T) {
	r := &Runner{}
	RegisterMigrations(r)
	found := false
	for _, m := range r.migrations {
		if m.Version == 10 {
			found = true
		}
	}
	if !found {
		t.Fatal("RecountVaultCounters not registered at version 10")
	}
	if MaxRegisteredVersion() < 10 {
		t.Fatalf("MaxRegisteredVersion = %d, want >= 10", MaxRegisteredVersion())
	}

	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ws := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	putEngramKeys(t, db, ws, 2)
	putCount(t, db, ws, 4)
	if err := writeMigrationVersion(db, 9); err != nil {
		t.Fatal(err)
	}
	r2 := NewRunner(db)
	RegisterMigrations(r2)
	if _, err := r2.Run(); err != nil {
		t.Fatal(err)
	}
	if got, _ := readCount(t, db, ws); got != 2 {
		t.Fatalf("data dir stamped at 9: counter = %d after Run, want 2", got)
	}
}
