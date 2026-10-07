package migrate

import (
	"encoding/binary"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// vaultCountKeyLen is 0x15 | ws(8). The keyspace registry lists 0x15 as this
// key's sole user; any other length found under 0x15 is reported, not touched.
const vaultCountKeyLen = 1 + 8

// engramKeyMinLen is 0x01 | ws(8) | id(16) — the same discriminator
// storage.countEngramsForVault uses, so the recount here and the lazy seed
// agree on what an engram key is.
const engramKeyMinLen = 1 + 8 + 16

// RecountVaultCounters rewrites every persisted per-vault engram counter
// (0x15|ws) to the number of engram keys (0x01|ws|id) the vault actually holds.
//
// Why this exists (G9). The counter is seeded lazily by a scan the first time
// a process touches a vault. Until the fix that ships with this migration, the
// write paths seeded it AFTER committing the write, so the seed scan already
// contained the write and the post-commit increment counted it again: the first
// batch a fresh vault ever received was double-counted (a first batch of 50
// reported 100; a first single write reported 2). The seeded value is then
// persisted and never re-derived, so every existing data dir carries the
// excess for good. On a production data dir, 45 of 49 small vaults showed a
// counter above every engram the vault could account for, never below.
//
// Fixing the write path stops new drift but corrects nothing already stored,
// so this recounts once. A recount is the same scan the lazy seed has always
// used, so it is the counter's own definition, not a new one: a count of
// 0x01 keys, which includes soft-deleted and archived engrams exactly as the
// write/delete accounting does.
//
// Failure mode is loud by construction: any scan or commit error fails the
// migration, which refuses startup and leaves the version unstamped, so the
// next start retries. Vaults with engrams but no persisted counter are left
// alone — the (now correct) lazy seed handles them on first touch.
//
// Idempotent: a second run finds every counter already equal to its recount
// and writes nothing. Runs before NewPebbleStore, so no in-memory counter or
// coalescer can race it.
func RecountVaultCounters(db *pebble.DB) error {
	// 1. Collect the persisted counters.
	type stored struct {
		ws    [8]byte
		value int64
		valid bool
	}
	var counters []stored
	unexpected := 0
	{
		iter, err := db.NewIter(&pebble.IterOptions{
			LowerBound: []byte{prefix.VaultCount},
			UpperBound: []byte{prefix.VaultCount + 1},
		})
		if err != nil {
			return fmt.Errorf("recount vault counters: new iter: %w", err)
		}
		for valid := iter.First(); valid; valid = iter.Next() {
			k := iter.Key()
			if len(k) != vaultCountKeyLen {
				unexpected++
				continue
			}
			var s stored
			copy(s.ws[:], k[1:9])
			if v := iter.Value(); len(v) == 8 {
				s.value = int64(binary.BigEndian.Uint64(v))
				s.valid = true
			}
			counters = append(counters, s)
		}
		iterErr := iter.Error()
		iter.Close()
		if iterErr != nil {
			return fmt.Errorf("recount vault counters: scan 0x15: %w", iterErr)
		}
	}
	if unexpected > 0 {
		slog.Warn("migration: unexpected keys under the vault-count prefix (0x15); left in place",
			"count", unexpected)
	}
	if len(counters) == 0 {
		slog.Info("migration: no persisted vault counters to recount")
		return nil
	}

	// 2. Count engram keys per vault in one pass over 0x01.
	actual := make(map[[8]byte]int64, len(counters))
	{
		iter, err := db.NewIter(&pebble.IterOptions{
			LowerBound: []byte{prefix.Engram},
			UpperBound: []byte{prefix.Engram + 1},
		})
		if err != nil {
			return fmt.Errorf("recount vault counters: new engram iter: %w", err)
		}
		for valid := iter.First(); valid; valid = iter.Next() {
			k := iter.Key()
			if len(k) < engramKeyMinLen {
				continue
			}
			var ws [8]byte
			copy(ws[:], k[1:9])
			actual[ws]++
		}
		iterErr := iter.Error()
		iter.Close()
		if iterErr != nil {
			return fmt.Errorf("recount vault counters: scan 0x01: %w", iterErr)
		}
	}

	// 3. Rewrite every counter that disagrees with its recount.
	batch := db.NewBatch()
	defer batch.Close()
	corrected := 0
	var over, under int64
	for _, s := range counters {
		n := actual[s.ws]
		if s.valid && s.value == n {
			continue
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(n))
		if err := batch.Set(keys.VaultCountKey(s.ws), buf[:], nil); err != nil {
			return fmt.Errorf("recount vault counters: stage: %w", err)
		}
		corrected++
		switch {
		case !s.valid:
			slog.Warn("migration: malformed vault counter replaced by recount",
				"ws", fmt.Sprintf("%x", s.ws), "recount", n)
		case s.value > n:
			over += s.value - n
		default:
			under += n - s.value
		}
		slog.Info("migration: vault counter corrected",
			"ws", fmt.Sprintf("%x", s.ws), "stored", s.value, "recount", n)
	}
	if corrected > 0 {
		if err := batch.Commit(pebble.Sync); err != nil {
			return fmt.Errorf("recount vault counters: commit: %w", err)
		}
	}
	slog.Info("migration: recounted vault counters (0x15)",
		"vaults", len(counters), "corrected", corrected,
		"overcount_removed", over, "undercount_added", under)
	return nil
}
