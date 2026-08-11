package auth

import "github.com/scrypster/muninndb/internal/prefix"

const (
	prefixAdminUser      = prefix.AdminUser
	prefixAPIKey         = prefix.APIKey
	prefixAPIKeyVIdx     = prefix.APIKeyVaultIdx
	prefixVaultCfg       = prefix.VaultConfig
	prefixCapability     = prefix.Capability
	prefixCapabilityVIdx = prefix.CapabilityVaultIdx
	// prefixAPIKeyGlob: glob-scope key index; see docs/key-space-schema.md.
	// Relocated from the inlined 0x29 (which collided with storage's
	// RecallEvent prefix) to the registry during the v0.10.0 merge.
	prefixAPIKeyGlob = prefix.APIKeyGlobIdx
)

// apiKeyGlobIdxKey indexes API keys that have at least one glob-scope entry
// (e.g. "proj-*"). Unlike apiKeyVaultIdxKey this is not vault-scoped — a glob
// entry has no single vault name to index against — so listing for a
// specific vault scans this whole (expected-small) prefix and pattern-matches
// each key's scope client-side (see Store.ListAPIKeys). The value is the
// key's 16-byte storage hash, mirroring apiKeyVaultIdxKey.
func apiKeyGlobIdxKey(keyID []byte) []byte {
	key := make([]byte, 1+8)
	key[0] = prefixAPIKeyGlob
	copy(key[1:], keyID[:8])
	return key
}

// apiKeyGlobIdxBounds returns the [lower, upper) scan range covering every
// glob-scope index entry.
func apiKeyGlobIdxBounds() (lower, upper []byte) {
	return []byte{prefixAPIKeyGlob}, []byte{prefixAPIKeyGlob + 1}
}

// APIKeyGlobIdxKey is the exported form of apiKeyGlobIdxKey. It exists so the
// v5 storage migration (internal/storage/migrate, which already imports this
// package — see v3's RelocateAuthPrefixes) can backfill 0x46 glob-index
// entries for keys written before the index existed, using the exact same
// key format as GenerateScopedAPIKey rather than a hand-copied replica.
func APIKeyGlobIdxKey(keyID []byte) []byte {
	return apiKeyGlobIdxKey(keyID)
}

func adminUserKey(username string) []byte {
	key := make([]byte, 1+len(username))
	key[0] = prefixAdminUser
	copy(key[1:], username)
	return key
}

func apiKeyStorageKey(hash16 []byte) []byte {
	key := make([]byte, 1+16)
	key[0] = prefixAPIKey
	copy(key[1:], hash16)
	return key
}

// apiKeyVaultIdxKey indexes keys by vault for listing/revocation.
func apiKeyVaultIdxKey(vault string, keyID []byte) []byte {
	key := make([]byte, 1+len(vault)+1+8)
	key[0] = prefixAPIKeyVIdx
	copy(key[1:], vault)
	key[1+len(vault)] = 0x00
	copy(key[1+len(vault)+1:], keyID[:8])
	return key
}

// apiKeyVaultIdxPrefix returns the scan prefix for all keys in a vault.
func apiKeyVaultIdxPrefix(vault string) []byte {
	key := make([]byte, 1+len(vault)+1)
	key[0] = prefixAPIKeyVIdx
	copy(key[1:], vault)
	key[1+len(vault)] = 0x00
	return key
}

func capabilityStorageKey(hash16 []byte) []byte {
	key := make([]byte, 1+16)
	key[0] = prefixCapability
	copy(key[1:], hash16)
	return key
}

// capabilityVaultIdxKey indexes capabilities by vault for listing/revocation.
func capabilityVaultIdxKey(vault string, capID []byte) []byte {
	key := make([]byte, 1+len(vault)+1+8)
	key[0] = prefixCapabilityVIdx
	copy(key[1:], vault)
	key[1+len(vault)] = 0x00
	copy(key[1+len(vault)+1:], capID[:8])
	return key
}

// capabilityVaultIdxPrefix returns the scan prefix for all capabilities in a vault.
func capabilityVaultIdxPrefix(vault string) []byte {
	key := make([]byte, 1+len(vault)+1)
	key[0] = prefixCapabilityVIdx
	copy(key[1:], vault)
	key[1+len(vault)] = 0x00
	return key
}

func vaultConfigKey(vault string) []byte {
	key := make([]byte, 1+len(vault))
	key[0] = prefixVaultCfg
	copy(key[1:], vault)
	return key
}

func vaultConfigUpperBound() []byte {
	return []byte{prefixVaultCfg + 1}
}
