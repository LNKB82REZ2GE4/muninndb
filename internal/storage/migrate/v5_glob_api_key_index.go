package migrate

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/prefix"
)

// BackfillAPIKeyGlobIndex is the v5 migration: repairs API keys whose scope
// contains a glob entry (e.g. "proj-*") but were written before the
// v0.10.0 merge relocated the glob-scope index from the inlined 0x29 byte
// (which upstream independently allocated to RecallEvent) to the dedicated
// prefix.APIKeyGlobIdx (0x46).
//
// Those keys' primary auth.APIKey records at prefix.APIKey (0x43) are fine —
// ValidateAPIKey does a direct hash lookup and is unaffected — but their
// glob-index entry is orphaned at the old 0x29 byte, invisible to
// ListAPIKeys/RevokeAPIKey, which now only scan 0x46. This migration:
//
//  1. Scans every APIKey record at 0x43 and, for each with a glob scope
//     entry (auth.APIKey.HasGlobScope), writes the 0x46 index entry via
//     auth.APIKeyGlobIdxKey — the same builder GenerateScopedAPIKey uses for
//     new keys — if not already present.
//  2. Deletes stale orphaned entries at the old 0x29 byte, but only ones
//     matching the old glob-index key's exact format: 1(prefix)+8(keyID) = 9
//     bytes. RecallEvent, which now legitimately owns 0x29, is always
//     1+8+16 = 25 bytes (see keys.RecallEventKey), so length alone
//     disambiguates the two record types with no ambiguity — any other
//     length at 0x29 is left untouched out of caution.
//
// Idempotent: step 1 no-ops when the 0x46 entry already exists; step 2
// no-ops once no 9-byte keys remain at 0x29.
func BackfillAPIKeyGlobIndex(db *pebble.DB) error {
	created, alreadyPresent, keysScanned, err := backfillGlobIndexEntries(db)
	if err != nil {
		return fmt.Errorf("backfill api key glob index: %w", err)
	}

	removed, err := pruneStaleGlobIndexEntries(db)
	if err != nil {
		return fmt.Errorf("backfill api key glob index: %w", err)
	}

	slog.Info("backfill api key glob index complete",
		"keys_scanned", keysScanned,
		"glob_entries_created", created,
		"glob_entries_already_present", alreadyPresent,
		"stale_0x29_entries_removed", removed,
	)
	return nil
}

// backfillGlobIndexEntries scans prefix.APIKey (0x43) and writes a
// prefix.APIKeyGlobIdx (0x46) entry for every glob-scoped key that is
// missing one.
func backfillGlobIndexEntries(db *pebble.DB) (created, alreadyPresent, keysScanned int, err error) {
	iter, iterErr := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.APIKey},
		UpperBound: []byte{prefix.APIKey + 1},
	})
	if iterErr != nil {
		return 0, 0, 0, fmt.Errorf("new iter (0x%02x): %w", prefix.APIKey, iterErr)
	}
	defer iter.Close()

	batch := db.NewBatch()
	defer batch.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		keysScanned++
		k := append([]byte(nil), iter.Key()...)

		val, valErr := iter.ValueAndErr()
		if valErr != nil {
			return created, alreadyPresent, keysScanned, fmt.Errorf("iter value at %x: %w", k, valErr)
		}

		var key auth.APIKey
		if jsonErr := json.Unmarshal(val, &key); jsonErr != nil {
			return created, alreadyPresent, keysScanned, fmt.Errorf("corrupt api key record at %x: %w", k, jsonErr)
		}
		if !key.HasGlobScope() {
			continue
		}
		if len(key.StorageHash) < 8 {
			return created, alreadyPresent, keysScanned, fmt.Errorf("api key %q (%x) has short storage hash (%d bytes)", key.ID, k, len(key.StorageHash))
		}

		// GenerateScopedAPIKey derives keyID as h[:8] and StorageHash as
		// h[:16] from the same SHA-256 digest, so StorageHash[:8] == keyID.
		keyID := key.StorageHash[:8]
		globKey := auth.APIKeyGlobIdxKey(keyID)

		if _, closer, getErr := db.Get(globKey); getErr == nil {
			closer.Close()
			alreadyPresent++
			continue
		} else if getErr != pebble.ErrNotFound {
			return created, alreadyPresent, keysScanned, fmt.Errorf("get %x: %w", globKey, getErr)
		}

		if setErr := batch.Set(globKey, append([]byte(nil), key.StorageHash...), nil); setErr != nil {
			return created, alreadyPresent, keysScanned, fmt.Errorf("set %x: %w", globKey, setErr)
		}
		created++
	}
	if iterErr := iter.Error(); iterErr != nil {
		return created, alreadyPresent, keysScanned, fmt.Errorf("iter (0x%02x): %w", prefix.APIKey, iterErr)
	}

	if created > 0 {
		if commitErr := batch.Commit(pebble.Sync); commitErr != nil {
			return created, alreadyPresent, keysScanned, fmt.Errorf("commit new glob-index entries: %w", commitErr)
		}
	}
	return created, alreadyPresent, keysScanned, nil
}

// oldGlobIdxKeyLen is the length of the pre-relocation glob-index key that
// used to live at prefix.RecallEvent (0x29): 1(prefix) + 8(keyID).
const oldGlobIdxKeyLen = 9

// recallEventKeyLen is keys.RecallEventKey's length: 1(prefix) + 8(ws) +
// 16(eventID). Duplicated here (rather than imported) to keep the length
// check a compile-time constant; internal/storage/migrate already imports
// internal/storage/keys for other migrations, so this stays trivially
// re-derivable if RecallEventKey's layout ever changes.
const recallEventKeyLen = 1 + 8 + 16

// pruneStaleGlobIndexEntries deletes leftover old-format (9-byte) glob-index
// entries at prefix.RecallEvent (0x29), leaving every 25-byte RecallEvent
// record — and any other length, out of caution — untouched.
func pruneStaleGlobIndexEntries(db *pebble.DB) (int, error) {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.RecallEvent},
		UpperBound: []byte{prefix.RecallEvent + 1},
	})
	if err != nil {
		return 0, fmt.Errorf("new iter (0x%02x): %w", prefix.RecallEvent, err)
	}

	var stale [][]byte
	for valid := iter.First(); valid; valid = iter.Next() {
		if k := iter.Key(); len(k) == oldGlobIdxKeyLen {
			stale = append(stale, append([]byte(nil), k...))
		}
	}
	iterErr := iter.Error()
	iter.Close()
	if iterErr != nil {
		return 0, fmt.Errorf("iter (0x%02x): %w", prefix.RecallEvent, iterErr)
	}

	if len(stale) == 0 {
		return 0, nil
	}

	batch := db.NewBatch()
	defer batch.Close()
	for _, k := range stale {
		if err := batch.Delete(k, nil); err != nil {
			return 0, fmt.Errorf("delete stale glob-index entry %x: %w", k, err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return 0, fmt.Errorf("commit stale glob-index deletes: %w", err)
	}
	return len(stale), nil
}
