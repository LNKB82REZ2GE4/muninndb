package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
)

// ErrKeyNotFound is returned by RevokeAPIKey when the key does not exist.
var ErrKeyNotFound = errors.New("api key not found")

// GenerateAPIKey creates a new API key for the given vault.
// Returns the raw token (shown once) and the key metadata.
// expiresAt is optional; pass nil for a key that never expires.
func (s *Store) GenerateAPIKey(vault, label, mode string, expiresAt *time.Time) (token string, key APIKey, err error) {
	if mode != ModeFull && mode != ModeObserve && mode != ModeWrite {
		err = fmt.Errorf("mode must be %q, %q, or %q", ModeFull, ModeObserve, ModeWrite)
		return
	}

	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		err = fmt.Errorf("generate random bytes: %w", err)
		return
	}
	token = "mk_" + base64.RawURLEncoding.EncodeToString(raw)

	h := sha256.Sum256(raw)
	storageHash := h[:16]
	keyID := h[:8]

	key = APIKey{
		ID:          base64.RawURLEncoding.EncodeToString(keyID),
		Vault:       vault,
		Label:       label,
		Mode:        mode,
		CreatedAt:   time.Now(),
		StorageHash: storageHash,
		ExpiresAt:   expiresAt,
	}

	data, marshalErr := json.Marshal(key)
	if marshalErr != nil {
		err = fmt.Errorf("marshal key: %w", marshalErr)
		return
	}

	batch := s.db.NewBatch()
	if setErr := batch.Set(apiKeyStorageKey(storageHash), data, nil); setErr != nil {
		batch.Close()
		err = setErr
		return
	}
	if setErr := batch.Set(apiKeyVaultIdxKey(vault, keyID), storageHash, nil); setErr != nil {
		batch.Close()
		err = setErr
		return
	}
	err = batch.Commit(pebble.Sync)
	return
}

// GenerateScopedAPIKey creates a new API key scoped to multiple vault
// entries. Each entry in scope is either a literal vault name or a
// "<literal-prefix>*" glob (see ValidateScopeEntry). allowAll permits a bare
// "*" entry, which matches every vault.
//
// Returns the raw token (shown once) and the key metadata. Every literal
// scope entry, plus a glob-scope index entry when scope contains any glob,
// is written atomically with the key record in a single Sync-tier batch.
func (s *Store) GenerateScopedAPIKey(scope []string, label, mode string, expiresAt *time.Time, allowAll bool) (token string, key APIKey, err error) {
	if mode != ModeFull && mode != ModeObserve && mode != ModeWrite {
		err = fmt.Errorf("mode must be %q, %q, or %q", ModeFull, ModeObserve, ModeWrite)
		return
	}
	if err = ValidateScope(scope, allowAll); err != nil {
		return
	}

	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		err = fmt.Errorf("generate random bytes: %w", err)
		return
	}
	token = "mk_" + base64.RawURLEncoding.EncodeToString(raw)

	h := sha256.Sum256(raw)
	storageHash := h[:16]
	keyID := h[:8]

	key = APIKey{
		ID:          base64.RawURLEncoding.EncodeToString(keyID),
		Vaults:      append([]string(nil), scope...),
		Label:       label,
		Mode:        mode,
		CreatedAt:   time.Now(),
		StorageHash: storageHash,
		ExpiresAt:   expiresAt,
	}
	// Legacy display convenience: surface the default vault (if any) through
	// the old single-value field so callers that only look at key.Vault
	// (admin UI, older clients) still show something sensible.
	if def, ok := key.DefaultVault(); ok {
		key.Vault = def
	}

	data, marshalErr := json.Marshal(key)
	if marshalErr != nil {
		err = fmt.Errorf("marshal key: %w", marshalErr)
		return
	}

	batch := s.db.NewBatch()
	if setErr := batch.Set(apiKeyStorageKey(storageHash), data, nil); setErr != nil {
		batch.Close()
		err = setErr
		return
	}
	seenLiteral := make(map[string]bool, len(scope))
	hasGlob := false
	for _, entry := range scope {
		if IsGlobScopeEntry(entry) {
			hasGlob = true
			continue
		}
		if seenLiteral[entry] {
			continue
		}
		seenLiteral[entry] = true
		if setErr := batch.Set(apiKeyVaultIdxKey(entry, keyID), storageHash, nil); setErr != nil {
			batch.Close()
			err = setErr
			return
		}
	}
	if hasGlob {
		if setErr := batch.Set(apiKeyGlobIdxKey(keyID), storageHash, nil); setErr != nil {
			batch.Close()
			err = setErr
			return
		}
	}
	err = batch.Commit(pebble.Sync)
	return
}

// ValidateAPIKey parses the token and returns the associated key metadata.
func (s *Store) ValidateAPIKey(token string) (APIKey, error) {
	const pfx = "mk_"
	if len(token) <= len(pfx) || token[:len(pfx)] != pfx {
		return APIKey{}, fmt.Errorf("invalid token format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[len(pfx):])
	if err != nil || len(raw) != 32 {
		return APIKey{}, fmt.Errorf("invalid token encoding")
	}
	h := sha256.Sum256(raw)
	data, closer, err := s.db.Get(apiKeyStorageKey(h[:16]))
	if err != nil {
		return APIKey{}, fmt.Errorf("invalid key")
	}
	defer closer.Close()

	var key APIKey
	if err := json.Unmarshal(data, &key); err != nil {
		return APIKey{}, fmt.Errorf("corrupt key record: %w", err)
	}
	if key.ExpiresAt != nil && time.Now().After(*key.ExpiresAt) {
		return APIKey{}, fmt.Errorf("api key has expired")
	}
	return key, nil
}

// ListAPIKeys returns all API key metadata for a vault (tokens not included).
// This includes keys whose scope names vault literally and keys whose scope
// contains a glob entry matching vault.
func (s *Store) ListAPIKeys(vault string) ([]APIKey, error) {
	seen := make(map[string]bool)
	var keys []APIKey

	fetch := func(storageHash []byte) *APIKey {
		data, closer, err := s.db.Get(apiKeyStorageKey(storageHash))
		if err != nil {
			return nil
		}
		defer closer.Close()
		var key APIKey
		if jsonErr := json.Unmarshal(data, &key); jsonErr != nil {
			return nil
		}
		return &key
	}

	// Literal scope index: exact vault match.
	prefix := apiKeyVaultIdxPrefix(vault)
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	upper[len(upper)-1]++

	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upper,
	})
	if err != nil {
		return nil, fmt.Errorf("new iter: %w", err)
	}
	for iter.First(); iter.Valid(); iter.Next() {
		storageHash := make([]byte, 16)
		copy(storageHash, iter.Value())
		if key := fetch(storageHash); key != nil && !seen[key.ID] {
			seen[key.ID] = true
			keys = append(keys, *key)
		}
	}
	if err := iter.Error(); err != nil {
		iter.Close()
		return nil, err
	}
	iter.Close()

	// Glob scope index: expected small; scan and pattern-match client-side.
	// Glob strings are never resolved through a vault-name lookup — only
	// compared as string prefixes against the concrete vault we're listing.
	globLower, globUpper := apiKeyGlobIdxBounds()
	globIter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: globLower,
		UpperBound: globUpper,
	})
	if err != nil {
		return nil, fmt.Errorf("new iter: %w", err)
	}
	defer globIter.Close()
	for globIter.First(); globIter.Valid(); globIter.Next() {
		storageHash := make([]byte, 16)
		copy(storageHash, globIter.Value())
		key := fetch(storageHash)
		if key == nil || seen[key.ID] {
			continue
		}
		if ScopeMatch(key.Scope(), vault) {
			seen[key.ID] = true
			keys = append(keys, *key)
		}
	}
	return keys, globIter.Error()
}

// RevokeAPIKey removes the key with the given display ID from the given
// vault. vault only needs to be one entry within the key's scope (or, for a
// key whose scope is glob-only, may be any value — the key is located via
// its keyID-only glob index entry regardless). Returns ErrKeyNotFound if the
// key does not exist or the ID is invalid.
//
// Revocation deletes the key record and every one of its secondary index
// entries (one per literal scope entry, plus the glob index entry if any) in
// a single Sync-tier batch.
func (s *Store) RevokeAPIKey(vault, keyID string) error {
	idBytes, err := base64.RawURLEncoding.DecodeString(keyID)
	if err != nil || len(idBytes) != 8 {
		return ErrKeyNotFound
	}

	var storageHash []byte
	if v, closer, getErr := s.db.Get(apiKeyVaultIdxKey(vault, idBytes)); getErr == nil {
		storageHash = append([]byte(nil), v...)
		closer.Close()
	} else if v, closer, getErr := s.db.Get(apiKeyGlobIdxKey(idBytes)); getErr == nil {
		storageHash = append([]byte(nil), v...)
		closer.Close()
	} else {
		return ErrKeyNotFound
	}

	data, closer, err := s.db.Get(apiKeyStorageKey(storageHash))
	if err != nil {
		return ErrKeyNotFound
	}
	var key APIKey
	jsonErr := json.Unmarshal(data, &key)
	closer.Close()
	if jsonErr != nil {
		return ErrKeyNotFound
	}

	batch := s.db.NewBatch()
	if err := batch.Delete(apiKeyStorageKey(storageHash), nil); err != nil {
		batch.Close()
		return err
	}
	for _, entry := range key.Scope() {
		if IsGlobScopeEntry(entry) {
			continue
		}
		if err := batch.Delete(apiKeyVaultIdxKey(entry, idBytes), nil); err != nil {
			batch.Close()
			return err
		}
	}
	if key.HasGlobScope() {
		if err := batch.Delete(apiKeyGlobIdxKey(idBytes), nil); err != nil {
			batch.Close()
			return err
		}
	}
	return batch.Commit(pebble.Sync)
}
