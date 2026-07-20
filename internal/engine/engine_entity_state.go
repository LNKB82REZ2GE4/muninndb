package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/scrypster/muninndb/internal/storage"
)

// errEntityPresent is the internal sentinel used to stop an entity reverse
// index scan as soon as one matching link in the target workspace is found.
var errEntityPresent = errors.New("entity present")

// entityPresentInVault reports whether entityName has at least one engram
// link in ws. The 0x1F entity registry is global/vault-agnostic
// (docs/plans/2026-07-19-project-vaults.md §4.1 correction), so every entity
// read or write path that accepts a caller-scoped vault must gate on this
// before touching or returning the global record — otherwise a caller can
// probe the existence of, or mutate the state of, an entity that only
// appears in a vault they have no access to.
func (e *Engine) entityPresentInVault(ctx context.Context, ws [8]byte, entityName string) (bool, error) {
	found := false
	err := e.store.ScanEntityEngrams(ctx, entityName, func(gotWS [8]byte, id storage.ULID) error {
		if gotWS == ws {
			found = true
			return errEntityPresent
		}
		return nil
	})
	if err != nil && !errors.Is(err, errEntityPresent) {
		return false, err
	}
	return found, nil
}

// maxMergeChainDepth bounds entityVisibleInVault's walk through MergedInto
// tombstone chains, guarding against a corrupt or cyclic chain looping forever.
const maxMergeChainDepth = 8

// entityVisibleInVault reports whether entityName should be visible to a
// caller scoped to ws — either because it still has a live engram link there
// (entityPresentInVault), or because it is a "merged" tombstone whose
// MergedInto target is itself visible in ws. A merge relinks every engram
// link from the losing entity to the winning one (see MergeEntity), so a
// freshly-merged entity has zero links left in any vault even though its
// tombstone record (state, MergedInto) is exactly what a repeat merge/state
// call needs to read to produce a correct "already merged" error instead of
// a misleading "not found" — this follows the chain so that error still
// surfaces only when the caller could otherwise see the entity.
func (e *Engine) entityVisibleInVault(ctx context.Context, ws [8]byte, entityName string) (bool, error) {
	seen := make(map[string]bool, maxMergeChainDepth)
	for depth := 0; depth < maxMergeChainDepth; depth++ {
		if seen[entityName] {
			return false, nil // cycle guard
		}
		seen[entityName] = true

		present, err := e.entityPresentInVault(ctx, ws, entityName)
		if err != nil {
			return false, err
		}
		if present {
			return true, nil
		}

		rec, err := e.store.GetEntityRecord(ctx, entityName)
		if err != nil {
			return false, err
		}
		if rec == nil || rec.State != "merged" || rec.MergedInto == "" {
			return false, nil
		}
		entityName = rec.MergedInto
	}
	return false, nil
}

// SetEntityState sets the lifecycle state of a named entity, and optionally
// corrects its type. For state="merged", mergedInto must be the canonical name.
// entityType may be empty — when empty the existing type is preserved.
//
// vault gates the entity record's own global visibility: entityName must
// actually appear in vault (see entityPresentInVault) or this call is
// rejected as not-found, even if the entity exists globally under another
// vault's data — the 0x1F registry is global, but callers are not.
func (e *Engine) SetEntityState(ctx context.Context, vault, entityName, state, mergedInto, entityType string) error {
	if entityName == "" {
		return fmt.Errorf("set_entity_state: entity_name is required")
	}

	ws := e.store.ResolveVaultPrefix(vault)
	present, err := e.entityVisibleInVault(ctx, ws, entityName)
	if err != nil {
		return fmt.Errorf("set_entity_state: check entity presence: %w", err)
	}
	if !present {
		return fmt.Errorf("set_entity_state: entity %q not found", entityName)
	}

	// Get existing to preserve other fields.
	existing, err := e.store.GetEntityRecord(ctx, entityName)
	if err != nil {
		return fmt.Errorf("set_entity_state: read entity: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("set_entity_state: entity %q not found", entityName)
	}

	// Use provided type; fall back to existing when caller omits it.
	resolvedType := existing.Type
	if entityType != "" {
		resolvedType = entityType
	}

	// Build updated record — UpsertEntityRecord will validate state and MergedInto consistency.
	record := storage.EntityRecord{
		Name:       entityName,
		State:      state,
		MergedInto: mergedInto,
		Type:       resolvedType,
		Confidence: existing.Confidence,
	}

	return e.store.UpsertEntityRecord(ctx, record, "mcp:entity_state")
}

// EntityStateOp is a single operation in a SetEntityStateBatch call.
type EntityStateOp struct {
	EntityName string
	State      string
	MergedInto string
	EntityType string
}

// SetEntityStateBatch applies multiple entity state updates sequentially, all
// gated to vault (see SetEntityState). Returns one error per operation
// (nil = success). Never returns a top-level error — partial success is
// preserved. Respects context cancellation between items.
func (e *Engine) SetEntityStateBatch(ctx context.Context, vault string, ops []EntityStateOp) []error {
	errs := make([]error, len(ops))
	for i, op := range ops {
		if ctx.Err() != nil {
			errs[i] = ctx.Err()
			continue
		}
		errs[i] = e.SetEntityState(ctx, vault, op.EntityName, op.State, op.MergedInto, op.EntityType)
	}
	return errs
}
