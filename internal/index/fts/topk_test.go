package fts

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// TestTopKScoredIDsMatchesFullSort pins the property the fast path has to
// preserve: selecting the topK must return exactly what sorting everything and
// truncating returns — same rows, same order, including on ties.
func TestTopKScoredIDsMatchesFullSort(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, n := range []int{0, 1, 5, 30, 31, 500} {
		for _, k := range []int{0, 1, 7, 30, 1000} {
			src := make([]ScoredID, n)
			for i := range src {
				var id [16]byte
				rng.Read(id[:])
				src[i] = ScoredID{ID: id, Score: float64(rng.Intn(4))} // heavy ties on purpose
			}

			want := append([]ScoredID(nil), src...)
			sortScoredIDs(want)
			if k > 0 && len(want) > k {
				want = want[:k]
			}

			got := topKScoredIDs(append([]ScoredID(nil), src...), k)
			if len(got) != len(want) {
				t.Fatalf("n=%d k=%d: got %d results, want %d", n, k, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("n=%d k=%d: row %d = %v, want %v", n, k, i, got[i], want[i])
				}
			}
		}
	}
}

// TestSortScoredIDsDeterministicOnTies pins the tie-break. Candidates are
// drained from a map with randomised iteration order, so without a total order
// two identically-scoring engrams swap places between identical queries.
func TestSortScoredIDsDeterministicOnTies(t *testing.T) {
	mk := func(b byte) ScoredID {
		var id [16]byte
		id[0] = b
		return ScoredID{ID: id, Score: 0.5}
	}
	a, b, c := mk(1), mk(2), mk(3)

	for _, in := range [][]ScoredID{{a, b, c}, {c, b, a}, {b, a, c}} {
		s := append([]ScoredID(nil), in...)
		sortScoredIDs(s)
		if s[0] != a || s[1] != b || s[2] != c {
			t.Fatalf("ties not ordered by ID ascending: %v", s)
		}
	}
}

// TestTopKScoredIDsIsNotQuadratic is the RED guard for the recall-latency
// regression: Search ranked its ENTIRE candidate set with an insertion sort
// before truncating to topK, so cost grew as O(matched²) while the answer
// needed only the top 30. On a 137k-engram production vault a query whose
// terms were common enough to match ~10^5 docs spent 20.0s of a 20.2s recall
// inside that one call (99.5% of CPU samples).
//
// 200k candidates is the scale the bug actually appeared at, and costs nothing
// to synthesise — no Pebble, no indexing. The bounded selection does it in
// single-digit milliseconds; the insertion sort takes minutes. The budget is
// deliberately loose so it fails on a return to O(n²), not on a slow machine.
func TestTopKScoredIDsIsNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("200k-candidate selection")
	}
	rng := rand.New(rand.NewSource(11))
	const n = 200_000
	s := make([]ScoredID, n)
	for i := range s {
		var id [16]byte
		rng.Read(id[:])
		s[i] = ScoredID{ID: id, Score: rng.Float64()}
	}

	start := time.Now()
	got := topKScoredIDs(s, 30)
	elapsed := time.Since(start)

	if len(got) != 30 {
		t.Fatalf("got %d results, want 30", len(got))
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return scoredIDLess(got[j], got[i]) }) {
		t.Fatalf("selection not in descending rank order")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("selecting top 30 of %d candidates took %v — the whole candidate set is being ranked again", n, elapsed)
	}
}

// TestSearchReturnsRankedTopK pins the end-to-end contract through Search: the
// bounded selection must still return exactly topK rows, in rank order, when
// the corpus matches far more docs than the caller asked for.
func TestSearchReturnsRankedTopK(t *testing.T) {
	if testing.Short() {
		t.Skip("indexing 4k engrams")
	}
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	var ws [8]byte
	ws[0] = 0xA1

	const docs = 4000
	for i := range docs {
		var id [16]byte
		id[0] = byte(i >> 8)
		id[1] = byte(i)
		// Every doc carries the same common term, so one query term alone
		// produces `docs` candidates for a topK of 30.
		if err := idx.IndexEngram(ws, id, "composite", "tester",
			fmt.Sprintf("common composite damage record number %d", i), nil); err != nil {
			t.Fatal(err)
		}
	}

	res, err := idx.Search(context.Background(), ws, "common composite damage", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 30 {
		t.Fatalf("got %d results, want 30", len(res))
	}
	if !sort.SliceIsSorted(res, func(i, j int) bool { return scoredIDLess(res[j], res[i]) }) {
		t.Fatalf("results not in descending rank order")
	}
}
