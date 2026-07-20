package auth

const (
	prefixAdminUser  byte = 0x11
	prefixAPIKey     byte = 0x12
	prefixAPIKeyVIdx byte = 0x13
	prefixVaultCfg   byte = 0x14
	prefixAPIKeyGlob byte = 0x29 // glob-scope key index; see docs/key-space-schema.md
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

func vaultConfigKey(vault string) []byte {
	key := make([]byte, 1+len(vault))
	key[0] = prefixVaultCfg
	copy(key[1:], vault)
	return key
}

func vaultConfigUpperBound() []byte {
	return []byte{prefixVaultCfg + 1}
}
