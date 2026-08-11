package migrate

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/prefix"
)

// TestBackfillAPIKeyGlobIndex_CreatesIndexAndPrunesStale seeds a pre-v5
// store: a glob-scoped apiKey record (0x43) whose glob-index entry is still
// orphaned at the old 0x29 byte, a fake 25-byte RecallEvent record that also
// lives at 0x29 and must survive untouched, and a non-glob apiKey that must
// not gain a 0x46 entry. It then asserts the migration is a no-op the second
// time it runs.
func TestBackfillAPIKeyGlobIndex_CreatesIndexAndPrunesStale(t *testing.T) {
	db := openTestDB(t)

	// Glob-scoped key: 16-byte storageHash, keyID = storageHash[:8] (mirrors
	// GenerateScopedAPIKey's h[:16]/h[:8] split off one SHA-256 digest).
	storageHash := make([]byte, 16)
	for i := range storageHash {
		storageHash[i] = byte(i + 1)
	}
	keyID := storageHash[:8]

	globKey := auth.APIKey{
		ID:          "glob-key",
		Vaults:      []string{"agent-memory", "proj-*"},
		Label:       "claude-agent-scoped",
		Mode:        auth.ModeFull,
		StorageHash: storageHash,
	}
	globKeyJSON, err := json.Marshal(globKey)
	if err != nil {
		t.Fatalf("marshal glob key: %v", err)
	}
	apiKeyKey := append([]byte{prefix.APIKey}, storageHash...)
	if err := db.Set(apiKeyKey, globKeyJSON, pebble.Sync); err != nil {
		t.Fatalf("seed apiKey record: %v", err)
	}

	// Orphaned old-format glob-index entry at 0x29: 1(prefix) + 8(keyID) = 9 bytes.
	oldGlobIdxKey := append([]byte{prefix.RecallEvent}, keyID...)
	if err := db.Set(oldGlobIdxKey, storageHash, pebble.Sync); err != nil {
		t.Fatalf("seed old glob-index entry: %v", err)
	}

	// Fake RecallEvent record at 0x29: 1(prefix) + 8(ws) + 16(eventID) = 25 bytes.
	recallEventKey := make([]byte, recallEventKeyLen)
	recallEventKey[0] = prefix.RecallEvent
	binary.BigEndian.PutUint64(recallEventKey[1:9], 0xAABBCCDD)
	for i := 9; i < recallEventKeyLen; i++ {
		recallEventKey[i] = byte(i)
	}
	recallEventVal := []byte("recall-event-payload")
	if err := db.Set(recallEventKey, recallEventVal, pebble.Sync); err != nil {
		t.Fatalf("seed recall event: %v", err)
	}

	// Non-glob key: must gain no 0x46 entry and its 0x43 record is untouched.
	nonGlobHash := make([]byte, 16)
	for i := range nonGlobHash {
		nonGlobHash[i] = byte(0x80 + i)
	}
	nonGlobKey := auth.APIKey{
		ID:          "plain-key",
		Vault:       "agent-memory",
		Mode:        auth.ModeFull,
		StorageHash: nonGlobHash,
	}
	nonGlobKeyJSON, err := json.Marshal(nonGlobKey)
	if err != nil {
		t.Fatalf("marshal non-glob key: %v", err)
	}
	nonGlobKeyRecordKey := append([]byte{prefix.APIKey}, nonGlobHash...)
	if err := db.Set(nonGlobKeyRecordKey, nonGlobKeyJSON, pebble.Sync); err != nil {
		t.Fatalf("seed non-glob apiKey record: %v", err)
	}

	if err := BackfillAPIKeyGlobIndex(db); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// 0x46 entry created with the correct format/value.
	wantGlobIdxKey := auth.APIKeyGlobIdxKey(keyID)
	v, closer, err := db.Get(wantGlobIdxKey)
	if err != nil {
		t.Fatalf("expected 0x46 entry at %x: %v", wantGlobIdxKey, err)
	}
	if !bytes.Equal(v, storageHash) {
		t.Errorf("0x46 entry value = %x, want %x", v, storageHash)
	}
	closer.Close()

	// Stale 9-byte 0x29 entry gone.
	if _, _, err := db.Get(oldGlobIdxKey); err != pebble.ErrNotFound {
		t.Errorf("stale old glob-index entry still present (err=%v)", err)
	}

	// RecallEvent entry at 0x29 untouched.
	v, closer, err = db.Get(recallEventKey)
	if err != nil {
		t.Fatalf("recall event was deleted: %v", err)
	}
	if !bytes.Equal(v, recallEventVal) {
		t.Errorf("recall event value = %q, want %q", v, recallEventVal)
	}
	closer.Close()

	// Non-glob key got no 0x46 entry.
	nonGlobGlobIdxKey := auth.APIKeyGlobIdxKey(nonGlobHash[:8])
	if _, _, err := db.Get(nonGlobGlobIdxKey); err != pebble.ErrNotFound {
		t.Errorf("non-glob key unexpectedly has a 0x46 entry (err=%v)", err)
	}

	// Second run is a no-op: same 0x46 entry, no error, nothing left to prune.
	if err := BackfillAPIKeyGlobIndex(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	v, closer, err = db.Get(wantGlobIdxKey)
	if err != nil {
		t.Fatalf("0x46 entry missing after second run: %v", err)
	}
	if !bytes.Equal(v, storageHash) {
		t.Errorf("0x46 entry value after second run = %x, want %x", v, storageHash)
	}
	closer.Close()
}
