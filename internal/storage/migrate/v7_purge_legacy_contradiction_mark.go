package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
)

// legacyDeclaredContradictionMarkPrefix is the byte this fork originally
// allocated to the per-vault declared-contradiction marker. It is NOT
// prefix.DeclaredContradictionMark any more and must never be referenced from
// the live key constructors — it exists here, as a literal, precisely because
// this migration's job is to erase the last traces of it.
const legacyDeclaredContradictionMarkPrefix byte = 0x2F

// legacyDeclaredContradictionMarkKeyLen is the exact length of the old marker
// key: 0x2F | ws(8). Length is the discriminator that makes this purge safe —
// see the doc comment on PurgeLegacyDeclaredContradictionMark.
const legacyDeclaredContradictionMarkKeyLen = 1 + 8

// PurgeLegacyDeclaredContradictionMark deletes every marker key this fork wrote
// under 0x2F and rebuilds the marker at its new home, 0x31.
//
// Why this exists. This fork allocated 0x2F for COG-29's per-vault
// declared-contradiction marker (migration v6). Upstream #726 subsequently
// relocated the replication keyspace onto the same byte, and #556 took 0x30 for
// the upsert forward index, so 0x2F/0x30 are now upstream's and this fork's
// marker moved to 0x31. A registry clash alone would be tolerable; leaving the
// old KEYS in place would not be. Upstream's replication layout partitions 0x2F
// on a second discriminator byte —
//
//	0x2F | 0x01 | seq_be64(8)  = 10 bytes  log entry
//	0x2F | 0x02 | name...                  replication metadata
//
// — while a legacy marker key is 0x2F | ws(8) = 9 bytes, where ws is a vault
// hash. Any vault whose hash begins 0x01 or 0x02 would leave a 9-byte record
// sitting inside upstream's log-entry or metadata sub-range, to be decoded as
// something it is not.
//
// ORDERING IS LOAD-BEARING. This migration is registered at version 7, ahead of
// the renumbered upstream replication relocation (version 8, upstream's own v5).
// The purge therefore runs while 0x2F still holds nothing but this fork's
// markers, and it is structurally impossible for it to delete a replication
// record. Do not reorder these two, and do not renumber this one upward.
//
// Safety. Only keys of exactly legacyDeclaredContradictionMarkKeyLen are
// deleted. That is belt-and-braces on top of the ordering guarantee: upstream's
// shortest 0x2F key is 10 bytes, so even a hypothetical out-of-order run could
// not reach a log entry, and metadata keys are 0x2F|0x02|name where name is
// non-empty. A key of any other length under 0x2F is left alone and counted, so
// an unexpected occupant is reported rather than silently removed.
//
// Idempotent: a second run finds no legacy keys and simply re-runs the (itself
// idempotent, monotone) backfill.
func PurgeLegacyDeclaredContradictionMark(db *pebble.DB) error {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{legacyDeclaredContradictionMarkPrefix},
		UpperBound: []byte{legacyDeclaredContradictionMarkPrefix + 1},
	})
	if err != nil {
		return fmt.Errorf("purge legacy declared-contradiction mark: new iter: %w", err)
	}

	var stale [][]byte
	unexpected := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != legacyDeclaredContradictionMarkKeyLen {
			unexpected++
			continue
		}
		stale = append(stale, append([]byte(nil), k...))
	}
	iterErr := iter.Error()
	iter.Close()
	if iterErr != nil {
		return fmt.Errorf("purge legacy declared-contradiction mark: scan: %w", iterErr)
	}

	if len(stale) > 0 {
		batch := db.NewBatch()
		defer batch.Close()
		for _, k := range stale {
			if err := batch.Delete(k, nil); err != nil {
				return fmt.Errorf("purge legacy declared-contradiction mark: delete: %w", err)
			}
		}
		if err := batch.Commit(pebble.Sync); err != nil {
			return fmt.Errorf("purge legacy declared-contradiction mark: commit: %w", err)
		}
	}

	if unexpected > 0 {
		slog.Warn("migration: unexpected keys left in the legacy 0x2F range",
			"count", unexpected,
			"note", "not this fork's marker shape (0x2F|ws(8)); left in place")
	}
	slog.Info("migration: purged legacy declared-contradiction markers (0x2F)",
		"deleted", len(stale), "unexpected_keys_left", unexpected)

	// Rebuild at 0x31. The backfill is the single source of truth for the
	// marker's content, so this relocates by recomputing rather than by copying
	// bytes across — a copy would carry forward any staleness the old keys had.
	return BackfillDeclaredContradictionMark(db)
}
