package storage

import (
	"hash/fnv"
	"sync"
)

const lockStripes = 256

// stripedMutex is a fixed-size array of mutexes for lock striping.
// Replaces unbounded sync.Map-based lock pools. Memory usage is constant at
// lockStripes × sizeof(sync.Mutex) ≈ 6 KB regardless of key count.
//
// Two different keys may map to the same mutex stripe (false sharing), but at
// 256 stripes this is negligible in practice and safe — contention causes
// brief waiting, never data corruption.
type stripedMutex struct {
	mu [lockStripes]sync.Mutex
}

// For returns the mutex for the given byte key using FNV-32a hashing.
func (s *stripedMutex) For(key []byte) *sync.Mutex {
	h := fnv.New32a()
	h.Write(key)
	return &s.mu[h.Sum32()%lockStripes]
}

// stripedRWMutex is stripedMutex with reader/writer semantics, for the case
// where the many-writers side is mutually compatible and only a rare
// read-modify-write needs exclusion. Same fixed-size, same false-sharing
// trade-off (~10 KB total).
//
// Its one user is the declared-contradiction marker: every path that writes a
// `contradicts` edge holds the READ lock across its batch commit (they do not
// conflict with each other — all of them write the identical `yes` byte),
// while the single path that can write `none` holds the WRITE lock across its
// check-then-set (see SetDeclaredContradictionMark).
type stripedRWMutex struct {
	mu [lockStripes]sync.RWMutex
}

// For returns the RWMutex for the given byte key using FNV-32a hashing.
func (s *stripedRWMutex) For(key []byte) *sync.RWMutex {
	h := fnv.New32a()
	h.Write(key)
	return &s.mu[h.Sum32()%lockStripes]
}
