package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// BackfillDeclaredContradictionMark writes the per-vault 0x2F declared-
// contradiction marker for every vault that already has associations, by
// scanning the forward-association keyspace (0x03) exactly once.
//
// Why this migration exists at all. COG-29's recall gate has to answer "may
// this vault contain a declared `contradicts` edge?" on every query. An
// association value carries its relation type, so no key prefix isolates
// contradicts edges and the only way to answer from the edges themselves is to
// scan them — which recall bounds at
// storage.DefaultDeclaredContradictionScanCap. On a vault whose associations
// exceed that cap the scan proves nothing, the gate correctly fails toward
// DOING the work, and that vault then pays the contradiction phase on every
// query for the life of the process, forever, with no way out. Measured on a
// 137k-engram production vault: 3.8M forward associations, cap hit on the
// first query after every restart.
//
// New writes maintain the marker atomically (storage.MarkDeclaredContradiction-
// InBatch, called in the same Pebble batch as the edge). This is the one-time
// EAGER backfill for edges written before the marker existed, and it is what
// makes the gate O(1) on a vault that is already too big to probe.
//
// The scan is uncapped ON PURPOSE — a capped backfill would write an unproven
// answer, and the only unsafe direction here is claiming "clean" without
// having looked. It costs one sequential pass over 0x03: measured at 0.77s for
// 3.8M associations, once, at startup.
//
// Vaults with NO associations at all never appear in the scan and get no
// marker. That is correct and deliberate: absent means UNKNOWN, the recall
// gate probes them (cheaply — they have nothing to scan) and persists the
// verdict itself.
//
// Idempotent: re-running rewrites the same one-byte values, and it never
// writes `none` over an existing `yes` — so a re-run cannot demote a vault
// whose contradicts edge was archived between passes.
func BackfillDeclaredContradictionMark(db *pebble.DB) error {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.AssocFwd},
		UpperBound: []byte{prefix.AssocFwd + 1},
	})
	if err != nil {
		return fmt.Errorf("backfill declared-contradiction mark: new iter: %w", err)
	}
	defer iter.Close()

	// Forward assoc key: 0x03(1) | ws(8) | src(16) | weightComplement(4) | dst(16)
	const assocKeyLen = 1 + 8 + 16 + 4 + 16

	// Vault order is not guaranteed to be contiguous across the scan in any way
	// this code should rely on, so accumulate per-vault verdicts and write once
	// at the end. One bool per vault — bounded by vault count, measured in bytes.
	found := make(map[[8]byte]bool)
	scanned, skipped := 0, 0

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) < assocKeyLen {
			skipped++
			continue
		}
		scanned++

		var ws [8]byte
		copy(ws[:], k[1:9])
		if found[ws] {
			continue // this vault is already settled as `yes`
		}
		val, valErr := iter.ValueAndErr()
		if valErr != nil {
			return fmt.Errorf("backfill declared-contradiction mark: read value: %w", valErr)
		}
		if storage.AssocValueRelType(val) == storage.RelContradicts {
			found[ws] = true
			continue
		}
		found[ws] = false
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("backfill declared-contradiction mark: scan: %w", err)
	}

	batch := db.NewBatch()
	defer batch.Close()
	yes, none, kept := 0, 0, 0
	for ws, hasContradiction := range found {
		mark := storage.DeclaredContradictionYes
		if !hasContradiction {
			// Monotonicity, enforced here rather than deferred to
			// storage.SetDeclaredContradictionMark because this path writes
			// through a raw batch: never demote a vault already marked `yes`.
			// A live scan sees only 0x03 edges, and a contradicts edge can be
			// archived (0x25) out from under it — the marker deliberately
			// outlives the edge.
			if cur, closer, err := db.Get(keys.DeclaredContradictionMarkKey(ws)); err == nil {
				already := len(cur) > 0 && cur[0] == storage.DeclaredContradictionYes
				closer.Close()
				if already {
					kept++
					continue
				}
			}
			mark = storage.DeclaredContradictionNone
			none++
		} else {
			yes++
		}
		if err := batch.Set(keys.DeclaredContradictionMarkKey(ws), []byte{mark}, nil); err != nil {
			return fmt.Errorf("backfill declared-contradiction mark: set: %w", err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("backfill declared-contradiction mark: commit: %w", err)
	}

	slog.Info("migration: backfilled declared-contradiction markers (0x2F)",
		"associations_scanned", scanned, "malformed_keys_skipped", skipped,
		"vaults_with_contradictions", yes, "vaults_clean", none, "vaults_left_marked", kept)
	return nil
}
