package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWrite_IdempotentID_ReturnsSameID verifies that two Write calls with the same
// idempotent_id return the same engram ID (idempotency guarantee on gRPC/REST path).
func TestWrite_IdempotentID_ReturnsSameID(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "default",
		Concept:      "test concept",
		Content:      "first write",
		IdempotentID: "op-abc-123",
	})
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	r2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "default",
		Concept:      "test concept",
		Content:      "first write",
		IdempotentID: "op-abc-123",
	})
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}

	if r1.ID != r2.ID {
		t.Errorf("expected same ID on idempotent re-write; got %q then %q", r1.ID, r2.ID)
	}
	if r2.Hint != "idempotent" {
		t.Errorf("expected Hint=%q on second write; got %q", "idempotent", r2.Hint)
	}
}

// TestWrite_IdempotentID_ChangedContent_ReturnsOriginal verifies that a re-submission
// with a different content but the same idempotent_id still returns the original engram.
// This is the correct idempotency semantic: the operation already ran.
func TestWrite_IdempotentID_ChangedContent_ReturnsOriginal(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "default",
		Concept:      "original concept",
		Content:      "original content",
		IdempotentID: "op-content-drift",
	})
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	r2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "default",
		Concept:      "updated concept",
		Content:      "different content entirely",
		IdempotentID: "op-content-drift",
	})
	if err != nil {
		t.Fatalf("second Write with changed content: %v", err)
	}

	if r1.ID != r2.ID {
		t.Errorf("expected original ID returned on content drift; got %q then %q", r1.ID, r2.ID)
	}
	if r2.Hint != "idempotent" {
		t.Errorf("expected Hint=%q; got %q", "idempotent", r2.Hint)
	}
}

// TestWriteBatch_IdempotentID_DedupsIntraBatch verifies that two items within the same
// batch sharing an idempotent_id produce only one engram, with the duplicate returning
// the first item's ID.
func TestWriteBatch_IdempotentID_DedupsIntraBatch(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resps, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{
		{
			Vault:        "default",
			Concept:      "first",
			Content:      "content a",
			IdempotentID: "op-batch-dedup",
		},
		{
			Vault:        "default",
			Concept:      "second",
			Content:      "content b",
			IdempotentID: "op-batch-dedup",
		},
	})

	for i, err := range errs {
		if err != nil {
			t.Errorf("item %d unexpected error: %v", i, err)
		}
	}
	if resps[0] == nil || resps[1] == nil {
		t.Fatal("expected both responses to be non-nil")
	}
	if resps[0].ID != resps[1].ID {
		t.Errorf("expected duplicate item to return same ID as first; got %q and %q", resps[0].ID, resps[1].ID)
	}
	if resps[1].Hint != "idempotent" {
		t.Errorf("expected duplicate item Hint=%q; got %q", "idempotent", resps[1].Hint)
	}
}

// TestWrite_IdempotentID_DifferentKeys verifies that different idempotent_ids produce
// distinct engrams (basic sanity check).
func TestWrite_IdempotentID_DifferentKeys(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "default",
		Concept:      "concept a",
		Content:      "content a",
		IdempotentID: "op-key-1",
	})
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	r2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:        "default",
		Concept:      "concept b",
		Content:      "content b",
		IdempotentID: "op-key-2",
	})
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}

	if r1.ID == r2.ID {
		t.Errorf("expected distinct IDs for different idempotency keys; both returned %q", r1.ID)
	}
}
