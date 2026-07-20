package mcp

// resolve_vault_scoped_test.go — tests for resolveVaultScoped, the
// multi-vault-aware successor to resolveVault used by dispatchToolCall.
// resolveVault itself is left untouched by the project-vaults change and
// keeps its own existing tests (see auth_mk_test.go).

import (
	"strings"
	"testing"
)

func TestResolveVaultScoped_NoArg_DefaultsToFirstLiteral(t *testing.T) {
	vault, errMsg := resolveVaultScoped([]string{"agent-memory", "proj-*"}, map[string]any{})
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if vault != "agent-memory" {
		t.Errorf("expected default 'agent-memory', got %q", vault)
	}
}

func TestResolveVaultScoped_ArgMatchesGlob(t *testing.T) {
	vault, errMsg := resolveVaultScoped([]string{"agent-memory", "proj-*"}, map[string]any{"vault": "proj-muninndb-abc123"})
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if vault != "proj-muninndb-abc123" {
		t.Errorf("expected 'proj-muninndb-abc123', got %q", vault)
	}
}

// TestResolveVaultScoped_ArgOutOfScope_NoLeak verifies that requesting a
// vault outside the key's scope is rejected, and that the error message does
// not echo any of the key's actual scope entries back to the caller.
func TestResolveVaultScoped_ArgOutOfScope_NoLeak(t *testing.T) {
	scope := []string{"agent-memory", "secret-project-vault"}
	_, errMsg := resolveVaultScoped(scope, map[string]any{"vault": "other-vault"})
	if errMsg == "" {
		t.Fatal("expected an error for an out-of-scope vault")
	}
	for _, entry := range scope {
		if strings.Contains(errMsg, entry) {
			t.Errorf("error message leaks scope entry %q: %s", entry, errMsg)
		}
	}
}

// TestResolveVaultScoped_NoArg_OutOfScopeDefault_NoLeak covers the case
// where the scope's default entry itself must never be echoed on some other
// failure path either — belt-and-braces alongside the ArgOutOfScope case.
func TestResolveVaultScoped_NoArg_OutOfScopeDefault_NoLeak(t *testing.T) {
	scope := []string{"agent-memory", "secret-project-vault"}
	_, errMsg := resolveVaultScoped(scope, map[string]any{"vault": "unrelated"})
	if errMsg == "" {
		t.Fatal("expected an error")
	}
	if strings.Contains(errMsg, "secret-project-vault") {
		t.Errorf("error message leaks non-default scope entry: %s", errMsg)
	}
}

// TestResolveVaultScoped_GlobFirst_NoArg_Errors covers a scope that begins
// with a glob: there is no literal default, so an argument-less call must
// error rather than silently picking something.
func TestResolveVaultScoped_GlobFirst_NoArg_Errors(t *testing.T) {
	_, errMsg := resolveVaultScoped([]string{"proj-*", "agent-memory"}, map[string]any{})
	if errMsg == "" {
		t.Fatal("expected an error when scope begins with a glob and no vault arg is given")
	}
	if strings.Contains(errMsg, "proj-*") || strings.Contains(errMsg, "agent-memory") {
		t.Errorf("error message leaks scope entries: %s", errMsg)
	}
}

func TestResolveVaultScoped_GlobFirst_ExplicitMatchingArg_Allowed(t *testing.T) {
	vault, errMsg := resolveVaultScoped([]string{"proj-*", "agent-memory"}, map[string]any{"vault": "proj-x"})
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if vault != "proj-x" {
		t.Errorf("expected 'proj-x', got %q", vault)
	}
}

// TestResolveVaultScoped_EmptyScope_UnpinnedBehavior verifies that a nil
// scope (static mdb_ token or open-server auth — no API key) behaves exactly
// like the legacy unpinned branch of resolveVault: explicit arg is honored
// verbatim, absent arg defaults to "default".
func TestResolveVaultScoped_EmptyScope_UnpinnedBehavior(t *testing.T) {
	vault, errMsg := resolveVaultScoped(nil, map[string]any{"vault": "custom"})
	if errMsg != "" || vault != "custom" {
		t.Fatalf("expected ('custom', \"\"), got (%q, %q)", vault, errMsg)
	}

	vault, errMsg = resolveVaultScoped(nil, map[string]any{})
	if errMsg != "" || vault != "default" {
		t.Fatalf("expected ('default', \"\"), got (%q, %q)", vault, errMsg)
	}
}

func TestResolveVaultScoped_InvalidVaultArg_Rejected(t *testing.T) {
	_, errMsg := resolveVaultScoped([]string{"agent-memory"}, map[string]any{"vault": "INVALID!"})
	if errMsg == "" {
		t.Fatal("expected an error for an invalid vault name")
	}
}
