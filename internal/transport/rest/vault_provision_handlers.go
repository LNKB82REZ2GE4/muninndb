package rest

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/scrypster/muninndb/internal/auth"
)

// CreateVaultRequest is the body for POST /api/vaults (project-vaults phase 2,
// §5.1). Provisioning rides the scoped API key surface — not the admin cookie
// session — so the muninn-hook can lazily create project vaults without ever
// carrying admin credentials.
type CreateVaultRequest struct {
	Name string `json:"name"`
	// Template names a plasticity preset (see auth.ValidPlasticityPreset),
	// e.g. "knowledge-graph" for project vaults, "reference" for durable
	// general-purpose vaults. Defaults to "default" when omitted.
	Template string `json:"template,omitempty"`
	// Optional built-in growth bounds and behavior, applied as plasticity
	// overrides on top of the template.
	RetentionDays        *float32 `json:"retention_days,omitempty"`
	MaxEngrams           *int     `json:"max_engrams,omitempty"`
	MultiUser            *bool    `json:"multi_user,omitempty"`
	BehaviorInstructions *string  `json:"behavior_instructions,omitempty"`
	// Public defaults to false (locked, requires an API key) when omitted —
	// matches the fail-closed default every other vault gets.
	Public *bool `json:"public,omitempty"`
}

// CreateVaultResponse wraps the resulting vault config plus whether this call
// created it (false when the vault already existed — idempotent no-op).
type CreateVaultResponse struct {
	Vault   auth.VaultConfig `json:"vault"`
	Created bool             `json:"created"`
}

// handleCreateVault implements POST /api/vaults: creates a vault and writes
// its 0x0E config from a template in one step. Authenticated by a scoped API
// key (not the admin cookie session) — full-mode keys may provision any
// vault matching their scope, which is what lets muninn-hook auto-provision
// proj-* vaults with only the agent key and no admin step.
//
// Idempotent: if the vault already has an explicitly persisted config, this
// call is a no-op that returns the existing config rather than clobbering it.
func (s *Server) handleCreateVault(authStore *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateVaultRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'name' is required")
			return
		}
		if !isValidVaultName(req.Name) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid vault name: must be 1-64 lowercase alphanumeric, hyphen, or underscore characters")
			return
		}

		// Full-mode requirement: observe/write-only keys never get to create
		// vaults, regardless of scope.
		mode, _ := r.Context().Value(auth.ContextMode).(string)
		if mode != auth.ModeFull {
			s.sendError(r, w, http.StatusForbidden, ErrVaultForbidden, "vault provisioning requires a full-mode key")
			return
		}

		// Scope check: nil scope (static token / admin bypass / open server)
		// means no restriction, matching resolveVaultScoped's convention
		// elsewhere in this fork. A scoped mk_ key must cover the requested name.
		scope := scopeFromContext(r.Context())
		if len(scope) > 0 && !auth.ScopeMatch(scope, req.Name) {
			s.sendError(r, w, http.StatusForbidden, ErrVaultForbidden, "vault mismatch: requested vault is outside this key's scope")
			return
		}

		template := req.Template
		if template == "" {
			template = "default"
		}
		if !auth.ValidPlasticityPreset(template) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, fmt.Sprintf("invalid template %q: must be a known plasticity preset", template))
			return
		}

		exists, err := authStore.VaultConfigExists(req.Name)
		if err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
			return
		}
		if exists {
			cfg, cfgErr := authStore.GetVaultConfig(req.Name)
			if cfgErr != nil {
				s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, cfgErr.Error())
				return
			}
			s.sendJSON(w, http.StatusOK, CreateVaultResponse{Vault: cfg, Created: false})
			s.EmitAudit(r, "vault.provision", "vault", req.Name, "exists", nil)
			return
		}

		plasticity := &auth.PlasticityConfig{Version: 1, Preset: template}
		if req.RetentionDays != nil {
			plasticity.RetentionDays = req.RetentionDays
		}
		if req.MaxEngrams != nil {
			plasticity.MaxEngrams = req.MaxEngrams
		}
		if req.MultiUser != nil {
			plasticity.MultiUser = req.MultiUser
		}
		if req.BehaviorInstructions != nil {
			plasticity.BehaviorInstructions = req.BehaviorInstructions
		}

		public := false
		if req.Public != nil {
			public = *req.Public
		}

		cfg := auth.VaultConfig{Name: req.Name, Public: public, Plasticity: plasticity}
		if err := authStore.SetVaultConfig(cfg); err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
			return
		}

		// Register the vault in the Pebble name index so admin operations
		// (reindex-fts, vault export) can find it before the first engram is
		// written. Mirrors handleSetVaultConfig's post-write step.
		type vaultNameRegistrar interface {
			RegisterVaultName(name string) error
		}
		if reg, ok := s.engine.(vaultNameRegistrar); ok {
			if rErr := reg.RegisterVaultName(cfg.Name); rErr != nil {
				slog.Warn("vault provision: failed to register vault name", "vault", cfg.Name, "err", rErr)
			}
		}

		s.sendJSON(w, http.StatusCreated, CreateVaultResponse{Vault: cfg, Created: true})
		s.EmitAudit(r, "vault.provision", "vault", req.Name, "ok", nil)
	}
}
