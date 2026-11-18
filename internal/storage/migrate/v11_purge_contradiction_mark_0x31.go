package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
)

// staleDeclaredContradictionMarkPrefix is the byte this fork used for the
// per-vault declared-contradiction marker between the 0.11.0 and 0.12.1
// merges. It is NOT prefix.DeclaredContradictionMark any more (that moved to
// 0xF0) and it now belongs to upstream's SupersedesRestoreMark (#952). It
// exists here, as a literal, because this migration's job is to erase this
// fork's records from it.
const staleDeclaredContradictionMarkPrefix byte = 0x31

// staleDeclaredContradictionMarkKeyLen is the exact length of the stale
// marker key: 0x31 | ws(8).
const staleDeclaredContradictionMarkKeyLen = 1 + 8

// PurgeDeclaredContradictionMarkAt0x31 deletes every declared-contradiction
// marker this fork wrote under 0x31 and rebuilds the marker at its new home,
// 0xF0.
//
// Why this exists. Upstream #952 allocated 0x31 to SupersedesRestoreMark: a
// per-vault watermark, key 0x31 | ws(8), value one byte holding the version
// of the one-time restore of archived supersedes edges that last completed
// over the vault (supersedesRestoreVersion, currently 1). This fork's marker
// had the BYTE-IDENTICAL shape — key 0x31 | ws(8), value 0x01 (declared) or
// 0x00 (proven clean). Left in place, a vault whose marker read 0x01 would
// look to upstream's decay pass as if the restore had already run at
// version 1, and its archived supersedes edges would never be restored. A
// marker reading 0x00 would merely look "not yet run" (harmless), but it is
// still a stranger's record in upstream's keyspace.
//
// THE SHAPE DOES NOT DISCRIMINATE, SO ORDERING DOES. Because the two record
// shapes coincide, no key/value test can tell an upstream restore mark at
// version 1 from this fork's "declared" marker. The purge is safe because of
// WHEN it runs, not what it matches:
//
//   - It is registered at version 11, above every version this fork has
//     applied, so it runs exactly once, on the first start of the first
//     binary that knows SupersedesRestoreMark, against a store whose stamp
//     (≤ 10) proves it was last opened by a binary that never wrote 0x31 as
//     upstream's mark. Fork binaries before this merge wrote ONLY markers
//     there; upstream binaries cannot have opened a fork store (they refuse
//     any stored version above their own max, 6).
//   - The runner runs in cmd/muninn/server.go (and muninn.go for the
//     embedded library) on the raw Pebble handle BEFORE storage.NewPebbleStore
//     and engine.NewEngine. Upstream's only reader/writer of 0x31 is
//     Engine.restoreArchivedSupersedesOnce, reached from the engine's decay
//     pass, which cannot start before the engine exists.
//
// The shape test (key exactly 9 bytes AND value exactly one byte in
// {0x00, 0x01}) is belt-and-braces on top of that ordering: anything else
// under 0x31 — including a hypothetical future upstream restore version 2 —
// is left in place and counted.
//
// The residual case — a store last written by an UPSTREAM binary (stamp ≤ 6,
// real restore marks at 0x31) being opened by this fork — would lose its
// restore marks here. That failure is the benign direction: the mark's
// absence makes upstream's restore run once more over the vault (a scan that
// restores nothing already restored), whereas keeping a stale 0x01 would
// skip the restore forever.
//
// Rebuild. The marker is recomputed at 0xF0 by BackfillDeclaredContradiction-
// Mark, the single source of truth for its content (as v7 did for the 0x2F
// relocation). Copying bytes across would carry forward any staleness; and
// absence would also be safe (ABSENT = unknown, which the recall gate resolves
// by probing), but on a multi-million-association vault the probe is capped
// and proves nothing, so an eager rebuild is what keeps the gate O(1).
//
// Idempotent: a second run finds nothing to purge and re-runs the (itself
// idempotent, monotone) backfill.
func PurgeDeclaredContradictionMarkAt0x31(db *pebble.DB) error {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{staleDeclaredContradictionMarkPrefix},
		UpperBound: []byte{staleDeclaredContradictionMarkPrefix + 1},
	})
	if err != nil {
		return fmt.Errorf("purge 0x31 declared-contradiction mark: new iter: %w", err)
	}

	var stale [][]byte
	unexpected := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		v := iter.Value()
		if !isStaleDeclaredContradictionMark(k, v) {
			unexpected++
			continue
		}
		stale = append(stale, append([]byte(nil), k...))
	}
	iterErr := iter.Error()
	iter.Close()
	if iterErr != nil {
		return fmt.Errorf("purge 0x31 declared-contradiction mark: scan: %w", iterErr)
	}

	if len(stale) > 0 {
		batch := db.NewBatch()
		defer batch.Close()
		for _, k := range stale {
			if err := batch.Delete(k, nil); err != nil {
				return fmt.Errorf("purge 0x31 declared-contradiction mark: delete: %w", err)
			}
		}
		if err := batch.Commit(pebble.Sync); err != nil {
			return fmt.Errorf("purge 0x31 declared-contradiction mark: commit: %w", err)
		}
	}

	if unexpected > 0 {
		slog.Warn("migration: unexpected records left under 0x31",
			"count", unexpected,
			"note", "not the fork marker shape (0x31|ws(8) -> 0x00/0x01); left in place")
	}
	slog.Info("migration: purged fork declared-contradiction markers off 0x31",
		"deleted", len(stale), "unexpected_records_left", unexpected)

	return BackfillDeclaredContradictionMark(db)
}

// isStaleDeclaredContradictionMark is the shape discriminator: a key of
// exactly 0x31 | ws(8) whose value is exactly one byte, DeclaredContradiction-
// None or DeclaredContradictionYes.
func isStaleDeclaredContradictionMark(k, v []byte) bool {
	if len(k) != staleDeclaredContradictionMarkKeyLen || k[0] != staleDeclaredContradictionMarkPrefix {
		return false
	}
	if len(v) != 1 {
		return false
	}
	return v[0] == storage.DeclaredContradictionNone || v[0] == storage.DeclaredContradictionYes
}
