package auth

import (
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

// openAuthTestDB opens an in-memory Pebble DB for auth tests.
func openAuthTestDB(t *testing.T) *pebble.DB {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open auth test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestAPIKey_CreateAndValidate creates an API key for vault "v1" and validates it.
func TestAPIKey_CreateAndValidate(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	token, key, err := s.GenerateAPIKey("v1", "test-label", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if key.Vault != "v1" {
		t.Errorf("expected vault 'v1', got %q", key.Vault)
	}

	got, err := s.ValidateAPIKey(token)
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if got.Vault != "v1" {
		t.Errorf("expected vault 'v1', got %q", got.Vault)
	}
}

// TestAPIKey_NotFound validates that a random non-existent key returns an error.
func TestAPIKey_NotFound(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	// A well-formed but non-existent token.
	fakeToken := "mk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, err := s.ValidateAPIKey(fakeToken)
	if err == nil {
		t.Fatal("expected error for non-existent API key, got nil")
	}
}

// TestAPIKey_RevokeIdempotent creates a key, revokes it, and revokes it again.
// The second revoke should return an error (key no longer exists) — which is
// acceptable behavior; the important thing is that it does not panic.
func TestAPIKey_RevokeIdempotent(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	_, key, err := s.GenerateAPIKey("vault-idem", "label", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	if err := s.RevokeAPIKey("vault-idem", key.ID); err != nil {
		t.Fatalf("first RevokeAPIKey: %v", err)
	}

	// Second revoke: key is already gone — we just verify it does not panic.
	_ = s.RevokeAPIKey("vault-idem", key.ID)
}

// TestAPIKey_RevokedKeyInvalid creates a key, revokes it, and verifies that
// ValidateAPIKey returns an error for the revoked token.
func TestAPIKey_RevokedKeyInvalid(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	token, key, err := s.GenerateAPIKey("vault-rev", "label", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	if err := s.RevokeAPIKey("vault-rev", key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}

	_, validateErr := s.ValidateAPIKey(token)
	if validateErr == nil {
		t.Fatal("expected ValidateAPIKey to return an error after revocation, got nil")
	}
}

// TestAPIKey_ExpiryNeverExpires creates a key with no expiry and verifies it is valid.
func TestAPIKey_ExpiryNeverExpires(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	token, _, err := s.GenerateAPIKey("vault-exp", "no-expiry", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, err := s.ValidateAPIKey(token); err != nil {
		t.Fatalf("key with nil expiry should always be valid, got: %v", err)
	}
}

// TestAPIKey_ExpiryFuture creates a key expiring in the future and verifies it validates.
func TestAPIKey_ExpiryFuture(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	future := time.Now().Add(24 * time.Hour)
	token, key, err := s.GenerateAPIKey("vault-exp", "future-key", "full", &future)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if key.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set")
	}
	if _, err := s.ValidateAPIKey(token); err != nil {
		t.Fatalf("key with future expiry should be valid, got: %v", err)
	}
}

// TestAPIKey_ExpiryPast creates a key that is already expired and verifies it is rejected.
func TestAPIKey_ExpiryPast(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	past := time.Now().Add(-1 * time.Hour)
	token, _, err := s.GenerateAPIKey("vault-exp", "expired-key", "full", &past)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if _, err := s.ValidateAPIKey(token); err == nil {
		t.Fatal("expected ValidateAPIKey to reject an expired key, got nil")
	}
}

// TestGenerateAPIKey_WriteModeAccepted verifies that "write" is a valid mode.
func TestGenerateAPIKey_WriteModeAccepted(t *testing.T) {
	s := NewStore(openAuthTestDB(t))
	_, _, err := s.GenerateAPIKey("default", "ingest-bot", "write", nil)
	if err != nil {
		t.Errorf("write mode should be accepted, got: %v", err)
	}
}

// TestGenerateAPIKey_InvalidModeRejected verifies that unknown modes are rejected.
func TestGenerateAPIKey_InvalidModeRejected(t *testing.T) {
	s := NewStore(openAuthTestDB(t))
	_, _, err := s.GenerateAPIKey("default", "bad", "superuser", nil)
	if err == nil {
		t.Error("expected error for invalid mode, got nil")
	}
}

// TestAPIKey_WrongVault creates a key for "vault-a" and verifies that
// ValidateAPIKey still succeeds (keys are global by token, not vault-scoped)
// but the returned key's vault is "vault-a", not "vault-b".
func TestAPIKey_WrongVault(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	token, _, err := s.GenerateAPIKey("vault-a", "label", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	got, err := s.ValidateAPIKey(token)
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	// The stored key is for vault-a. If a caller checks got.Vault against "vault-b",
	// they would see a mismatch. This test asserts the vault field is correct.
	if got.Vault != "vault-a" {
		t.Errorf("expected vault 'vault-a', got %q", got.Vault)
	}
	if got.Vault == "vault-b" {
		t.Error("key should not be valid for vault-b")
	}
}

// --- GenerateScopedAPIKey: multi-vault index round-trip (§3.5) ---

func TestGenerateScopedAPIKey_MixedScope_CreateListRevoke(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	token, key, err := s.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "hook", ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	if len(key.Vaults) != 2 {
		t.Fatalf("expected 2 scope entries, got %v", key.Vaults)
	}
	if key.Vault != "agent-memory" {
		t.Errorf("expected legacy Vault display field to default to 'agent-memory', got %q", key.Vault)
	}

	// Validate: token round-trips to the same scope.
	got, err := s.ValidateAPIKey(token)
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if !ScopeMatch(got.Scope(), "proj-anything") {
		t.Error("expected validated key's scope to match proj-* glob")
	}

	// List by literal vault: found via the literal index (0x44).
	literalKeys, err := s.ListAPIKeys("agent-memory")
	if err != nil {
		t.Fatalf("ListAPIKeys(agent-memory): %v", err)
	}
	if !containsKeyID(literalKeys, key.ID) {
		t.Errorf("expected key %s to appear when listing agent-memory", key.ID)
	}

	// List by glob-matched vault: found via the glob index (0x46), never via
	// the literal index (since "proj-anything" was never written literally).
	globKeys, err := s.ListAPIKeys("proj-anything")
	if err != nil {
		t.Fatalf("ListAPIKeys(proj-anything): %v", err)
	}
	if !containsKeyID(globKeys, key.ID) {
		t.Errorf("expected key %s to appear when listing proj-anything via glob index", key.ID)
	}

	// Listing an out-of-scope vault must not find the key.
	otherKeys, err := s.ListAPIKeys("unrelated-vault")
	if err != nil {
		t.Fatalf("ListAPIKeys(unrelated-vault): %v", err)
	}
	if containsKeyID(otherKeys, key.ID) {
		t.Error("key must not appear for an out-of-scope vault")
	}

	// Revoke via one of the literal scope entries deletes every index entry
	// (literal 0x44 row and the glob 0x46 row).
	if err := s.RevokeAPIKey("agent-memory", key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, err := s.ValidateAPIKey(token); err == nil {
		t.Error("expected revoked key to be invalid")
	}
	if postRevoke, _ := s.ListAPIKeys("agent-memory"); containsKeyID(postRevoke, key.ID) {
		t.Error("revoked key must not appear in literal-scope listing")
	}
	if postRevoke, _ := s.ListAPIKeys("proj-anything"); containsKeyID(postRevoke, key.ID) {
		t.Error("revoked key must not appear in glob-scope listing (glob index entry not cleaned up)")
	}
}

func TestGenerateScopedAPIKey_GlobOnlyScope_RevokeByAnyVault(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	_, key, err := s.GenerateScopedAPIKey([]string{"proj-*"}, "hook", ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	// A glob-only scope has no literal index entry at all; revocation must
	// still succeed via the keyID-only glob index, regardless of the vault
	// argument passed in (it has no meaning for a glob-only key).
	if err := s.RevokeAPIKey("default", key.ID); err != nil {
		t.Fatalf("RevokeAPIKey on glob-only scope: %v", err)
	}
}

func TestGenerateScopedAPIKey_GlobGrammar_Rejected(t *testing.T) {
	s := NewStore(openAuthTestDB(t))

	cases := [][]string{
		{"proj-*-suffix"}, // mid-string star
		{"*"},             // bare star without allow-all
		{"Invalid_Upper*"},
		{"has space"},
	}
	for _, scope := range cases {
		if _, _, err := s.GenerateScopedAPIKey(scope, "l", ModeFull, nil, false); err == nil {
			t.Errorf("expected GenerateScopedAPIKey(%v) to be rejected", scope)
		}
	}

	// Bare '*' accepted only with allowAll.
	if _, _, err := s.GenerateScopedAPIKey([]string{"*"}, "l", ModeFull, nil, true); err != nil {
		t.Errorf("expected bare '*' with allowAll to succeed, got: %v", err)
	}
}

func TestGenerateScopedAPIKey_EmptyScope_Rejected(t *testing.T) {
	s := NewStore(openAuthTestDB(t))
	if _, _, err := s.GenerateScopedAPIKey(nil, "l", ModeFull, nil, false); err == nil {
		t.Error("expected error for empty scope")
	}
}

func containsKeyID(keys []APIKey, id string) bool {
	for _, k := range keys {
		if k.ID == id {
			return true
		}
	}
	return false
}
