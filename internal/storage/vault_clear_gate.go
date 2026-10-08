package storage

import (
	"bytes"
	"sort"
	"sync"
)

// vaultClearGate returns the per-vault RWMutex that serializes ClearVault
// against every path that mutates the vault's engram counter (STO-6a).
//
// Counter-mutating paths hold it SHARED from their pre-commit counter load
// through the commit, the counter Add, and their provenance Submit; ClearVault
// holds it EXCLUSIVE from its count capture to its last cache eviction. A
// write therefore lands either wholly before a clear (its engram is
// tombstoned and its count discarded with the evicted counter) or wholly
// after it (the counter re-seeds from a scan of the cleared keyspace and the
// write's Add is its only count). Without it, a write racing a clear of the
// SAME vault left the counter drifted in ~all of 200 forced rounds: a reader
// re-seeding between the counter eviction and the range tombstone counted
// engrams the tombstone then removed, and a write committing after the
// tombstone added its +1 to an already-evicted counter.
//
// The map grows by one small mutex per vault prefix ever written in this
// process; vault cardinality is small and the entry is reused on name reuse.
func (ps *PebbleStore) vaultClearGate(ws [8]byte) *sync.RWMutex {
	if g, ok := ps.vaultGates.Load(ws); ok {
		return g.(*sync.RWMutex)
	}
	g, _ := ps.vaultGates.LoadOrStore(ws, &sync.RWMutex{})
	return g.(*sync.RWMutex)
}

// holdVaultsAgainstClear takes the shared side of every listed vault's clear
// gate, deduplicated and in ascending prefix order (a fixed order keeps two
// multi-vault batches and two clears from forming a cycle through Go's
// writer-preferring RWMutex), and returns the release.
func (ps *PebbleStore) holdVaultsAgainstClear(wss ...[8]byte) func() {
	if len(wss) == 1 {
		g := ps.vaultClearGate(wss[0])
		g.RLock()
		return g.RUnlock
	}
	uniq := make([][8]byte, 0, len(wss))
	seen := make(map[[8]byte]struct{}, len(wss))
	for _, ws := range wss {
		if _, ok := seen[ws]; ok {
			continue
		}
		seen[ws] = struct{}{}
		uniq = append(uniq, ws)
	}
	sort.Slice(uniq, func(i, j int) bool { return bytes.Compare(uniq[i][:], uniq[j][:]) < 0 })
	gates := make([]*sync.RWMutex, len(uniq))
	for i, ws := range uniq {
		gates[i] = ps.vaultClearGate(ws)
		gates[i].RLock()
	}
	return func() {
		for i := len(gates) - 1; i >= 0; i-- {
			gates[i].RUnlock()
		}
	}
}
