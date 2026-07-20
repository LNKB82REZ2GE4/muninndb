package auth

import (
	"fmt"
	"strings"
)

// IsGlobScopeEntry reports whether entry is a trailing-star glob (e.g. "proj-*"),
// including the degenerate bare "*" case. It does not validate grammar — use
// ValidateScopeEntry for that.
func IsGlobScopeEntry(entry string) bool {
	return strings.HasSuffix(entry, "*")
}

// ValidateScopeEntry validates a single API key scope entry: either a literal
// vault name (see IsValidVaultName) or a "<literal-prefix>*" glob with exactly
// one trailing star and no star elsewhere. A bare "*" (match every vault) is
// only accepted when allowAll is true.
func ValidateScopeEntry(entry string, allowAll bool) error {
	if entry == "*" {
		if !allowAll {
			return fmt.Errorf("bare '*' scope entry requires --allow-all")
		}
		return nil
	}
	if IsGlobScopeEntry(entry) {
		prefix := strings.TrimSuffix(entry, "*")
		if prefix == "" || strings.Contains(prefix, "*") {
			return fmt.Errorf("invalid scope entry %q: only a single trailing '*' is allowed", entry)
		}
		if !IsValidVaultName(prefix) {
			return fmt.Errorf("invalid scope entry %q: prefix must be 1-64 lowercase alphanumeric, hyphen, underscore characters", entry)
		}
		return nil
	}
	if !IsValidVaultName(entry) {
		return fmt.Errorf("invalid scope entry %q: must be 1-64 lowercase alphanumeric, hyphen, underscore characters", entry)
	}
	return nil
}

// ValidateScope validates every entry in scope. scope must be non-empty.
func ValidateScope(scope []string, allowAll bool) error {
	if len(scope) == 0 {
		return fmt.Errorf("scope must contain at least one vault entry")
	}
	for _, entry := range scope {
		if err := ValidateScopeEntry(entry, allowAll); err != nil {
			return err
		}
	}
	return nil
}

// ScopeMatch reports whether vault is covered by scope: an exact literal
// match, or a prefix match against a "<prefix>*" glob entry (bare "*"
// matches everything).
func ScopeMatch(scope []string, vault string) bool {
	for _, entry := range scope {
		if entry == "*" {
			return true
		}
		if IsGlobScopeEntry(entry) {
			if strings.HasPrefix(vault, strings.TrimSuffix(entry, "*")) {
				return true
			}
			continue
		}
		if entry == vault {
			return true
		}
	}
	return false
}

// ResolveScopedVault determines the effective vault for a key's scope and an
// optionally-supplied request vault. reqVault == "" means the caller did not
// specify a vault explicitly.
//
//   - reqVault absent: the default is the first scope entry, iff it is a
//     literal (not a glob). A scope that begins with a glob has no default
//     and resolution fails.
//   - reqVault present: allowed iff it matches any scope entry (literal
//     equality or glob prefix); otherwise resolution fails.
//
// Returns ("", false) on failure. Callers must not echo scope contents back
// in error messages on failure — only the vault the caller itself supplied
// (if any) is safe to report back.
func ResolveScopedVault(scope []string, reqVault string) (string, bool) {
	if len(scope) == 0 {
		return "", false
	}
	if reqVault == "" {
		first := scope[0]
		if IsGlobScopeEntry(first) {
			return "", false
		}
		return first, true
	}
	if ScopeMatch(scope, reqVault) {
		return reqVault, true
	}
	return "", false
}

// Scope returns the key's effective vault scope: Vaults if set, otherwise a
// single-entry scope derived from the legacy Vault field (back-compat for
// records written before multi-vault keys existed).
func (k APIKey) Scope() []string {
	if len(k.Vaults) > 0 {
		return k.Vaults
	}
	if k.Vault != "" {
		return []string{k.Vault}
	}
	return nil
}

// DefaultVault returns the key's default vault (the first scope entry) and
// true, or ("", false) if the scope is empty or begins with a glob.
func (k APIKey) DefaultVault() (string, bool) {
	scope := k.Scope()
	if len(scope) == 0 {
		return "", false
	}
	if IsGlobScopeEntry(scope[0]) {
		return "", false
	}
	return scope[0], true
}

// HasGlobScope reports whether any scope entry is a glob pattern.
func (k APIKey) HasGlobScope() bool {
	for _, entry := range k.Scope() {
		if IsGlobScopeEntry(entry) {
			return true
		}
	}
	return false
}
