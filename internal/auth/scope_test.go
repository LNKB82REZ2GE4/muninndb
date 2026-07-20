package auth

import "testing"

func TestScopeMatch_Literal(t *testing.T) {
	scope := []string{"agent-memory", "other"}
	if !ScopeMatch(scope, "agent-memory") {
		t.Error("expected literal match")
	}
	if ScopeMatch(scope, "agent-memories") {
		t.Error("literal entries must not partial-match")
	}
}

func TestScopeMatch_Glob(t *testing.T) {
	scope := []string{"proj-*"}
	if !ScopeMatch(scope, "proj-muninndb-abc123") {
		t.Error("expected glob prefix match")
	}
	if ScopeMatch(scope, "other-vault") {
		t.Error("glob must not match unrelated vault")
	}
	if !ScopeMatch(scope, "proj-") {
		t.Error("glob prefix itself should match (empty suffix)")
	}
}

func TestScopeMatch_BareStar(t *testing.T) {
	scope := []string{"*"}
	if !ScopeMatch(scope, "anything-at-all") {
		t.Error("bare '*' should match every vault")
	}
}

func TestValidateScopeEntry_Literal(t *testing.T) {
	if err := ValidateScopeEntry("agent-memory", false); err != nil {
		t.Errorf("expected valid literal, got: %v", err)
	}
	if err := ValidateScopeEntry("Invalid_Upper", false); err == nil {
		t.Error("expected error for uppercase vault name")
	}
}

func TestValidateScopeEntry_TrailingGlob(t *testing.T) {
	if err := ValidateScopeEntry("proj-*", false); err != nil {
		t.Errorf("expected valid trailing-star glob, got: %v", err)
	}
}

func TestValidateScopeEntry_MidStringStar_Rejected(t *testing.T) {
	if err := ValidateScopeEntry("proj-*-suffix", false); err == nil {
		t.Error("expected error for mid-string '*'")
	}
	if err := ValidateScopeEntry("pro**", false); err == nil {
		t.Error("expected error for double-star")
	}
}

func TestValidateScopeEntry_BareStar_RequiresAllowAll(t *testing.T) {
	if err := ValidateScopeEntry("*", false); err == nil {
		t.Error("expected bare '*' to be rejected without allowAll")
	}
	if err := ValidateScopeEntry("*", true); err != nil {
		t.Errorf("expected bare '*' to be accepted with allowAll, got: %v", err)
	}
}

func TestValidateScopeEntry_InvalidChars_Rejected(t *testing.T) {
	cases := []string{"has space", "has/slash", "", "UPPER-*"}
	for _, c := range cases {
		if err := ValidateScopeEntry(c, true); err == nil {
			t.Errorf("expected error for invalid entry %q", c)
		}
	}
}

func TestValidateScope_EmptyRejected(t *testing.T) {
	if err := ValidateScope(nil, false); err == nil {
		t.Error("expected error for empty scope")
	}
}

func TestResolveScopedVault_NoArg_DefaultsToFirstLiteral(t *testing.T) {
	vault, ok := ResolveScopedVault([]string{"agent-memory", "proj-*"}, "")
	if !ok || vault != "agent-memory" {
		t.Errorf("expected default 'agent-memory', got (%q, %v)", vault, ok)
	}
}

func TestResolveScopedVault_NoArg_GlobFirst_NoDefault(t *testing.T) {
	_, ok := ResolveScopedVault([]string{"proj-*", "agent-memory"}, "")
	if ok {
		t.Error("expected no default when scope begins with a glob")
	}
}

func TestResolveScopedVault_ArgMatchesLiteral(t *testing.T) {
	vault, ok := ResolveScopedVault([]string{"a", "b"}, "b")
	if !ok || vault != "b" {
		t.Errorf("expected match on 'b', got (%q, %v)", vault, ok)
	}
}

func TestResolveScopedVault_ArgMatchesGlob(t *testing.T) {
	vault, ok := ResolveScopedVault([]string{"agent-memory", "proj-*"}, "proj-muninndb-abc123")
	if !ok || vault != "proj-muninndb-abc123" {
		t.Errorf("expected glob match, got (%q, %v)", vault, ok)
	}
}

func TestResolveScopedVault_ArgOutOfScope_Rejected(t *testing.T) {
	_, ok := ResolveScopedVault([]string{"agent-memory", "proj-*"}, "unrelated-vault")
	if ok {
		t.Error("expected rejection for out-of-scope vault")
	}
}

func TestResolveScopedVault_EmptyScope_AlwaysFails(t *testing.T) {
	if _, ok := ResolveScopedVault(nil, ""); ok {
		t.Error("expected failure for empty scope with no arg")
	}
	if _, ok := ResolveScopedVault(nil, "anything"); ok {
		t.Error("expected failure for empty scope with arg")
	}
}

func TestAPIKey_Scope_LegacyFallback(t *testing.T) {
	k := APIKey{Vault: "v1"}
	scope := k.Scope()
	if len(scope) != 1 || scope[0] != "v1" {
		t.Errorf("expected legacy scope [v1], got %v", scope)
	}
}

func TestAPIKey_Scope_PrefersVaultsField(t *testing.T) {
	k := APIKey{Vault: "v1", Vaults: []string{"a", "b"}}
	scope := k.Scope()
	if len(scope) != 2 || scope[0] != "a" || scope[1] != "b" {
		t.Errorf("expected scope [a b], got %v", scope)
	}
}

func TestAPIKey_DefaultVault_GlobFirst(t *testing.T) {
	k := APIKey{Vaults: []string{"proj-*", "agent-memory"}}
	if _, ok := k.DefaultVault(); ok {
		t.Error("expected no default vault when scope begins with a glob")
	}
}

func TestAPIKey_HasGlobScope(t *testing.T) {
	k := APIKey{Vaults: []string{"agent-memory", "proj-*"}}
	if !k.HasGlobScope() {
		t.Error("expected HasGlobScope to be true")
	}
	legacy := APIKey{Vault: "v1"}
	if legacy.HasGlobScope() {
		t.Error("legacy single-vault key must not report glob scope")
	}
}
