package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestStartReembedVault_VaultNotFound verifies that StartReembedVault with a
// vault that does not exist returns an error wrapping ErrVaultNotFound.
func TestStartReembedVault_VaultNotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.StartReembedVault(ctx, "does-not-exist", "test-model")
	if err == nil {
		t.Fatal("expected error for nonexistent vault, got nil")
	}
	if !errors.Is(err, ErrVaultNotFound) {
		t.Errorf("expected error to wrap ErrVaultNotFound, got: %v", err)
	}
}

// TestStartReembedVault_Success writes engrams, sets embed flags, then runs
// the reembed pipeline and verifies the flags are cleared and the job completes.
func TestStartReembedVault_Success(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "reembed-success"
	const DigestEmbed uint16 = 0x02

	// Write a few engrams.
	idStrings := make([]string, 3)
	for i := range idStrings {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vaultName,
			Concept: "concept",
			Content: "content for reembed test",
		})
		if err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
		idStrings[i] = resp.ID
	}

	// Parse string IDs to storage.ULID for flag operations.
	storeIDs := make([]storage.ULID, len(idStrings))
	for i, s := range idStrings {
		parsed, err := storage.ParseULID(s)
		if err != nil {
			t.Fatalf("ParseULID[%d]: %v", i, err)
		}
		storeIDs[i] = parsed
	}

	// Set embed flags on each engram.
	for i, id := range storeIDs {
		if err := eng.store.SetDigestFlag(ctx, id, DigestEmbed); err != nil {
			t.Fatalf("SetDigestFlag[%d]: %v", i, err)
		}
	}

	// Call StartReembedVault.
	job, err := eng.StartReembedVault(ctx, vaultName, "test-model")
	if err != nil {
		t.Fatalf("StartReembedVault: %v", err)
	}
	if job == nil {
		t.Fatal("StartReembedVault returned nil job")
	}
	if job.ID == "" {
		t.Error("expected non-empty job ID")
	}

	// Wait for job to complete.
	finalJob := waitForJob(t, eng, job.ID, 5*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		t.Fatalf("reembed job status = %s, want %s; err: %s",
			finalJob.GetStatus(), vaultjob.StatusDone, finalJob.GetErr())
	}

	// Verify: embed flags are cleared.
	for i, id := range storeIDs {
		flags, err := eng.store.GetDigestFlags(ctx, id)
		if err != nil {
			t.Fatalf("GetDigestFlags[%d] after reembed: %v", i, err)
		}
		if flags&DigestEmbed != 0 {
			t.Errorf("engram %d still has DigestEmbed set after reembed", i)
		}
	}
}

// TestRetryEmbedFailed_VaultNotFound mirrors TestStartReembedVault_VaultNotFound.
func TestRetryEmbedFailed_VaultNotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.RetryEmbedFailed(ctx, "does-not-exist", []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	if err == nil {
		t.Fatal("expected error for nonexistent vault, got nil")
	}
	if !errors.Is(err, ErrVaultNotFound) {
		t.Errorf("expected error to wrap ErrVaultNotFound, got: %v", err)
	}
}

// TestRetryEmbedFailed_ClearsOnlyListedEngrams pins the surgical-recovery
// path (COG-embed-blacklist P4): a caller can clear DigestEmbed/
// DigestEmbedFailed for a specific, known-bad set of engram IDs without
// touching every other engram in the vault — the cheap alternative to a full
// StartReembedVault when only a small set of engrams were wrongly stranded.
func TestRetryEmbedFailed_ClearsOnlyListedEngrams(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "retry-embed-failed"
	const DigestEmbed uint16 = 0x02
	const DigestEmbedFailed uint16 = 0x80

	idStrings := make([]string, 3)
	for i := range idStrings {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vaultName,
			Concept: "concept",
			Content: fmt.Sprintf("content for retry-embed-failed test #%d", i),
		})
		if err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
		idStrings[i] = resp.ID
	}
	storeIDs := make([]storage.ULID, len(idStrings))
	for i, s := range idStrings {
		parsed, err := storage.ParseULID(s)
		if err != nil {
			t.Fatalf("ParseULID[%d]: %v", i, err)
		}
		storeIDs[i] = parsed
	}

	// Strand all three, as the blacklisting bug would have.
	for i, id := range storeIDs {
		if err := eng.store.SetDigestFlag(ctx, id, DigestEmbedFailed); err != nil {
			t.Fatalf("SetDigestFlag[%d]: %v", i, err)
		}
	}

	// Recover only the first one.
	cleared, err := eng.RetryEmbedFailed(ctx, vaultName, []string{idStrings[0]})
	if err != nil {
		t.Fatalf("RetryEmbedFailed: %v", err)
	}
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1", cleared)
	}

	flags, err := eng.store.GetDigestFlags(ctx, storeIDs[0])
	if err != nil {
		t.Fatalf("GetDigestFlags[0]: %v", err)
	}
	if flags&DigestEmbedFailed != 0 || flags&DigestEmbed != 0 {
		t.Errorf("recovered engram still has embed flags set: %08b", flags)
	}

	for i := 1; i < len(storeIDs); i++ {
		flags, err := eng.store.GetDigestFlags(ctx, storeIDs[i])
		if err != nil {
			t.Fatalf("GetDigestFlags[%d]: %v", i, err)
		}
		if flags&DigestEmbedFailed == 0 {
			t.Errorf("engram %d must remain stranded (only [0] was requested), flags=%08b", i, flags)
		}
	}
}
