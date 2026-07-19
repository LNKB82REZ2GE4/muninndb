# Project Vaults — Two-Tier Agent Memory

**Status:** designed, not implemented. Awaiting Jake's review.
**Date:** 2026-07-19
**Scope:** fork feature (this repo) + local tooling (`muninn-hook`, `muninn-backfill`, `muninn-enricher`, new `muninn-promoter`). Built to upstream quality; **not** PR'd upstream for now (option kept open after real-world use).

---

## 1. Goal and memory model

Today every agent (Claude Code, Codex, Pi) shares one vault, `agent-memory`, for all work. This design adds **per-project vaults** alongside it, producing a two-tier hierarchy:

| Tier | Vault(s) | Content | Lifecycle |
|------|----------|---------|-----------|
| General | `agent-memory` | Durable, cross-project facts, preferences, decisions | Permanent, curated |
| Project | `proj-<slug>-<hash8>` | Complete per-project detail: turns, compactions, subagent output, project decisions | Archivable/deletable when project ends |

Inside a project, agents recall from **both** tiers (project-weighted); outside any project, behaviour is unchanged. Project vaults double as a hygiene layer: raw session noise stops accumulating in `agent-memory`.

> **Revision note:** after a full read of `docs/` (three parallel reader agents, 2026-07-19), the design was corrected in place: score-normalised merging replaced with weighted RRF (§4.2), the global entity registry acknowledged (§4.1), provisioning moved off the admin surface onto a new scoped `POST /api/vaults` (§5.1), and key-space/durability/migration constraints folded into §3.3, backfill fidelity into §5.2, promoter reuse into §5.4, archival into §7.

### Prior art in this repo (audited 2026-07-19 — none of this is implemented)

- `docs/auth.md` §"If you need isolation, use separate vaults" explicitly recommends a vault per project; keys, however, remain strictly one-vault (§3 fixes that).
- `docs/architecture.md` "Vault sharding" describes federating results "when multi-vault activation is requested" — aspirational sharding prose with no code behind it (no federation/multi-activate exists). §4's `ActivateMulti` is the single-node realisation of that stated direction, which strengthens the eventual upstream-PR case.
- MQL (`internal/query/mql/`) is called a "multi-vault query language" but its `FROM` clause takes exactly one vault, and the package is not wired to any transport (only its own tests import it). It is not an additional vault-authorization surface today; if it ever gets wired up, its `FROM` vault must pass the §3.2 scope check.

### Decision record (pinned with Jake, 2026-07-19)

1. **Project identity:** git root, with a registry override that can group several repos into one vault. Non-git directories → global-only.
2. **Provisioning:** automatic on first session in any repo; throwaway vaults are cheap to delete. Historical **backfill** from Claude/Codex/Pi transcripts plus existing `agent-memory` engrams.
3. **Write routing:** hook auto-stores (turns, compactions, subagent output) → **project vault only**. Deliberate `muninn_remember` → **both** vaults. A **promotion daemon** (LM Studio, same infra as the enricher) periodically distils project engrams worth elevating into `agent-memory` — daily when the model is loaded, plus on demand.
4. **Auth/recall:** fork feature — **multi-vault API keys** (scope lists with `proj-*` glob) and **`vaults[]` merged recall**, built upstream-quality with tests and fail-closed semantics; kept fork-local for now.
5. **Recall mix:** project-weighted (~2/3 project, ~1/3 general), labelled sections.
6. **Enricher:** sweeps **all** vaults.
7. **`agent-memory` rebuild:** deferred — decide after project backfill is proven complete.

---

## 2. Vault naming and project resolution

### 2.1 Names

```
proj-<slug>-<hash8>
```

- `slug`: git-root basename, lowercased, non-`[a-z0-9-_]` chars collapsed to `-`, truncated so the whole name ≤ 64 chars (server constraint: `1-64 [a-z0-9-_]`).
- `hash8`: first 8 hex chars of sha256 of the **absolute git-root path**. Prevents collisions between same-named directories; makes the name stable across renames of nothing but the basename... (if the directory *moves*, the registry pins the old vault — see below).

### 2.2 Resolution algorithm (in `muninn-hook`)

```
resolve_project(cwd):
  root = git -C cwd rev-parse --show-toplevel   (worktrees: use --git-common-dir's parent
                                                 so all worktrees of a repo share one vault)
  if registry maps root (or any parent of root) → use mapped vault name
  elif root found → proj-<slug>-<hash8(root)>
  else → None (global-only session)
```

### 2.3 Registry override

New optional block in `~/.config/muninn-hook.json`:

```json
"projects": {
  "/home/jake/thesis":            "proj-thesis-shared",
  "/home/jake/analysis/cscan":    "proj-thesis-shared",
  "/home/jake/scratch/junk":      null
}
```

- Values group multiple roots into one shared vault, or `null` to opt a path out entirely.
- Longest-prefix match wins. Registry names must still satisfy the server's vault-name grammar.

---

## 3. Fork feature A — multi-vault API keys

### 3.1 Data model (`internal/auth/types.go`)

```go
type APIKey struct {
    ID     string   `json:"id"`
    Vault  string   `json:"vault,omitempty"`  // legacy single-vault pin (kept for back-compat)
    Vaults []string `json:"vaults,omitempty"` // NEW: scope list; entries are literal names
                                              // or prefix globs ("proj-*")
    ...
}
```

- **Back-compat:** existing stored keys have only `Vault`; treat as `Vaults == [Vault]`. No storage migration required (JSON round-trips). New keys write `Vaults` and leave `Vault` set to the first literal entry (or empty when the scope starts with a glob) so old binaries reading the record fail closed rather than open.
- **Glob grammar (fail-closed, deliberately minimal):** a scope entry is either a literal vault name or `<literal-prefix>*` — a single trailing `*`, nothing else (no `?`, no mid-string `*`, no bare `*` unless explicitly `--vault '*'` which is refused by `api-key create` without `--allow-all`). Matching is plain prefix comparison; entries are validated against the vault-name grammar (minus the trailing star) at creation time.

### 3.2 Scope resolution (`internal/mcp/context.go` + REST middleware)

`resolveVault(pinned, args)` becomes `resolveVaultScoped(scope []string, args)`:

1. Arg absent → **default vault** = first entry of the scope list *iff it is a literal* (a scope that begins with a glob has no default; the call errors with "this key requires an explicit vault"). For Jake's agent keys the scope is `["agent-memory", "proj-*"]`, so the no-arg default stays `agent-memory` — **existing behaviour is unchanged for every current caller.**
2. Arg present → allowed iff it matches any scope entry (literal equality or glob prefix). On mismatch, the existing error text is reused and **must not leak** scope entries (preserves the current no-leak test, `TestResolveVault_PinnedVault_ArgDiffers_Error`).
3. Invalid vault-name grammar in the arg → same fail-closed rejection as today.

The same scope check is applied wherever `ContextVault` is populated today (REST middleware, MCP session auth, gRPC per-item vault resolution from #557/#558 — the v0.8.0 `ListVaults`/`BatchForget` RPCs already do per-item resolution, so they get the scope check in one place).

**Vault existence and globs:** a glob scope does **not** confer vault *creation* rights beyond what the mode allows. `full`-mode keys may write to (and thereby lazily create) any vault matching their scope — this is precisely what lets the hook auto-provision `proj-*` vaults with the agent key and no admin step. `observe`/`write` mode semantics are unchanged and orthogonal.

### 3.3 Secondary index (`internal/auth/keys.go`)

`apiKeyVaultIdxKey(vault, keyID)` currently indexes one entry per key under its literal vault (used by per-vault listing/revocation). Change:

Key↔vault becomes a **membership relation**, not a widened field — admin listing/revocation paths that assume one vault per key must be updated together:

- One index entry **per scope entry**, glob entries indexed under their literal pattern string (e.g. `proj-*`).
- `api-key list --vault X` returns keys whose scope contains literal `X` **plus** keys whose glob matches `X` (glob entries scanned; there will be a handful, not thousands).
- Revocation deletes all of a key's index entries.
- **Single `pebble.Batch` for key record + all index entries** — auth writes are Sync tier (`durability-guarantees.md`: zero data-loss window, one fsync per commit); per-entry writes would multiply synchronous fsyncs and lose atomicity.
- **Glob entries are patterns, never vaults:** they must never reach any vault-name→wsPrefix SipHash lookup (that would materialise a phantom workspace for the literal string `proj-*`). Everywhere a concrete vault is required, glob strings are rejected by validation.
- Any new key-space prefix must follow `key-space-schema.md`'s eight rules (assembly in `storage/keys.go`, doc-table row in the same PR, durability tier declared, idempotent numbered migration). Low prefix bytes are nearly exhausted — allocate from `0x25`/`0x28`, and resolve the documented `0x22` table ambiguity (referenced in prose, missing from the table) before claiming anything.
- **One-way migration:** schema versioning blocks binary downgrades once a new-format record is written (`cluster-operations.md` §6). Take a Pebble backup before the first scoped key is created; rollback to a pre-fork binary is otherwise impossible.
- **No-leak audit:** auth.md documents current error paths that 404-wrap `ErrVaultNotFound` and can echo vault names. The scoped-key change must sweep these paths so neither scope contents nor other vaults' existence is probeable.
- **Key handling logistics:** `mk_` secrets are shown once and revocation is immediate with no grace period — re-issuing the agent keys is a hard cutover. Sequence: create scoped keys → write `muninn-hook.json` → verify all hook flows live → revoke old keys.

### 3.4 CLI (`cmd/muninn/api_key.go`)

```
muninn api-key create --vaults agent-memory,proj-* --label claude-agent --mode full
```

`--vault` (singular) kept as an alias for a one-entry `--vaults`. `api-key list` prints the scope list.

### 3.5 Tests (extend `internal/mcp/auth_mk_test.go`, `internal/auth/*`)

- Legacy key (Vault only) behaves exactly as before (pin, mismatch error, no-leak).
- Scoped key: no-arg → first literal; arg in scope (literal + glob) → allowed; arg out of scope → error without leaking scope.
- Glob-first scope with no literal → no-arg call errors.
- Glob grammar rejection at creation (mid-string `*`, bare `*` without `--allow-all`, invalid chars).
- Index round-trip: create/list/revoke with mixed literal+glob scope.
- mdb\_ static token: unchanged (full access, no scope).

---

## 4. Fork feature B — `vaults[]` merged recall

### 4.1 API surface

`muninn_recall` / `muninn_activate` (MCP), `POST /api/activate` (REST), and the gRPC equivalent gain an optional `vaults: []string` argument, mutually exclusive with `vault`:

```json
{ "vaults": ["proj-muninndb-3f2a1b9c", "agent-memory"],
  "weights": [0.67, 0.33],          // optional, same length; default equal
  "context": [...], "limit": 12 }
```

Every entry must pass the key-scope check (§3.2); one failure fails the whole call (no partial leaks). `vaults[]` rides the request **body** (matching auth.md's stated direction: `?vault=` query param is canonical for single-vault, body-vault deprecated — an array has no clean query-string form anyway). `muninn_remember` (and `remember_batch`) accept the same `vaults[]` to write one engram **per listed vault** (independent IDs; each write is the existing single-vault path — this is how "deliberate remembers go to both" is implemented; note each write is a separate Sync-tier fsync, so dual-write remember costs ~2× latency — acceptable because deliberate remembers are low-volume). Cross-vault *engram links* (`link`, `consolidate`, trees) stay single-vault: engram IDs are meaningful only within a vault.

**Correction from the full-docs audit:** the design's earlier claim that "nothing joins across vaults" was wrong — the **entity registry (prefix `0x1F`) is global and vault-agnostic**. Shared entity names already connect engrams across vaults, so promoted engrams and their project-vault sources are entity-joined for free (promotion provenance = tags **plus** shared entities). Flip side: entity reads are a potential cross-vault information surface — the fork must confirm entity queries filter results to the caller's vault scope, or the no-leak property (§3.2) is violated by probing entity names.

### 4.2 Engine implementation

No changes inside `activateCore` (it is workspace-scoped by design; the vault dimension guard from v0.8.0 stays per-vault). Add a thin wrapper:

```go
func (e *Engine) ActivateMulti(ctx, reqs []*mbp.ActivateRequest, weights []float64) (*mbp.ActivateResponse, error)
```

- Runs `activateCore` per vault (serially first; they share the Pebble store — parallelism is a later optimisation, not a correctness matter).
- Each activation is tagged with its source vault in the response items (new `vault` field on the activation result — additive, omitted in single-vault responses for wire-compat).
- **Merge by rank, not score (revised after full-docs audit).** Activation scores are `ContentMatch × softplus(BaseLevel + Hebbian + Transition) × Confidence` with vault-scoped BM25 corpus statistics (`retrieval-design.md`), so raw scores are not comparable across vaults — and normalising each vault to its own top hit makes a 20-engram project vault's best "equal" a 5000-engram vault's best, reducing the tiers to nothing but the weights. Fuse by **weighted Reciprocal Rank Fusion** instead (RRF is already used in the retrieval pipeline's Phase 2 — reuse it): `score(e) = w_vault / (k + rank_in_vault(e))`, then cut to `limit` with the ≥1-slot-per-tier floor.
- **Shared budget:** BFS traversal is capped at 500 nodes *per activation* (`architecture.md` §3); a naive N-vault loop gets N×500 with no shared cap. `ActivateMulti` takes one result/traversal budget split across vaults.
- **PAS caveat:** Hebbian/predictive-activation state is per-vault; each vault's activation records its own transitions, which is correct for engrams in that vault, but implementation must not double-count one logical recall as multiple session events where PAS statistics feed injection gating.
- **Precondition:** comparable scores require every vault on the same embed model. The v0.8.0 dimension guard makes violations visible (a mismatched vault degrades to BM25-only); `ActivateMulti` surfaces a `degraded_vaults` field in the response rather than silently blending.

### 4.3 Tests

- Merge math: weights, per-vault normalisation, floor slot, limit.
- Scope enforcement: one out-of-scope vault fails the call.
- Wire compat: single-vault responses byte-identical to today.
- Dimension-mismatch vault → flagged degraded, not dropped.

---

## 5. Local tooling changes

### 5.1 `muninn-hook`

The single integration point — all six hook events for all three agents flow through it.

- **Key scheme:** re-issue the three per-agent keys with scope `agent-memory,proj-*` (replaces per-project key minting entirely — one key per agent, exactly as today, config unchanged in shape).
- **SessionStart (`orient`):** resolve project (§2.2). Provisioning **cannot** ride the admin surface — `SetVaultConfig` lives behind the admin cookie session, and the hook must not carry admin credentials. Instead the fork adds `POST /api/vaults` (name + template), allowed for **full-mode keys whose scope covers the requested name**: it creates the vault and writes its `0x0E` config from a template in one step — plasticity preset (`knowledge-graph` for project vaults vs `reference` for `agent-memory`), optional `retention_days`/`max_engrams` as a built-in growth bound, `multi_user: false` (so `where_left_off` stays meaningful per-project; consider `true` on shared `agent-memory`), and per-vault `behavior_instructions` carrying the store policy so any connecting agent gets it from `muninn_guide` at runtime rather than only from CLAUDE.md. Result cached in hook state (one-time cost). FTS indexing is async (~100 ms lag) — provisioning must not assume immediate searchability of the first write.
- **Recall (`orient`, `prompt`, `posttool`):** inside a project, one `activate` call with `vaults:[proj, agent-memory], weights:[0.67, 0.33]`, rendered as two labelled sections ("Project memories" / "General memories") — using `brief_mode` for compact injection rather than full engrams. `where_left_off` has no cross-vault primitive; the hook merges two O(limit) per-vault calls by last-access client-side. Outside a project: unchanged single-vault call. Server ports/paths are documented inconsistently across docs — always read them from `muninn-hook.json`, never hardcode.
- **Store (`store-turn`, `store-compact`, `store-subagent`, `store-message`, `flush`):** inside a project, target the **project vault only** (plain `vault:` arg — no API change needed). Outside: `agent-memory` as today. Engrams gain tags `project:<vault-name>` and keep the existing `source:*`/`op:<hash>` contract so the enricher and dedup work unchanged.
- **No MCP config changes:** the MCP server keys are the same re-scoped agent keys, so `muninn_remember(vaults:[...])` and project-scoped recall work through the existing single `muninn` server entry. CLAUDE.md gets a short store-policy note (auto: project; deliberate remember: both by default inside a project).

### 5.2 `muninn-backfill` v2

Existing architecture is kept (sqlite resumable queue, per-message `op:<sha8>` idempotency tags, `backfill` tag, structural-only writes, server-side dedup). Changes:

- **Vault routing:** extract `cwd` per session — Claude: per-message `cwd` field (also recoverable from the `~/.claude/projects/<slug>` directory name); Codex: `cwd` in the rollout header line; Pi: `cwd` in the session header. Route each message to `resolve_project(cwd)`'s vault (same code as the hook — extract `resolve_project` into a small shared module or vendored function). Sessions with no project → skipped (their content already reached `agent-memory` in the original backfill).
- **Completeness mode:** the original agent-memory backfill used an aggressive noise filter; detail was deliberately dropped. Project backfill runs with a **relaxed filter** (`--min-chars` lowered, fewer skip patterns) — project vaults are meant to be complete, and the noise cost is contained to the project tier. The exact filter deltas are a tuning decision at implementation time; both thresholds stay flag-controlled.
- **Fourth source — existing `agent-memory` engrams:** a `--from-vault agent-memory` pass copies engrams whose tags/entities identify a project (`op:` hash match against transcript-derived engrams prevents double-ingest: transcripts are the **primary** source; the vault-copy pass only fills gaps where a transcript no longer exists on disk). Copied engrams keep their original `created_at` and gain `backfill:vault-copy` provenance tags.
- **Dedup guarantee:** within a project vault, the same `op:<sha8(content)>` from transcript-pass and vault-copy-pass collides by design → server-side content dedup + idempotent tag makes re-runs and overlapping sources safe. Additionally use the documented `idempotent_id` field (first-class dedup key) rather than relying on tags alone. Nothing is ever *deleted* from `agent-memory` by backfill (its rebuild is a separate, deferred decision — §7).
- **Faithful history (from docs audit):** always set `created_at` to the historical timestamp (explicitly supported) so ACT-R base-level decay treats backfilled memories as old rather than freshly reinforced. Chunk batch writes at the documented max of 50 (each batch counts as one rate-limit event). Where the transcript already yields a summary/entities, supply inline enrichment with `caller_preferred` so the enricher doesn't reprocess months of history. Pace the run: the auto-association worker pool (4 workers, 1024-job buffer) drops jobs silently under sustained load — watch its metrics during large backfills.

### 5.3 `muninn-enricher`

- Sweep **all vaults**: enumerate via the v0.8.0 gRPC `ListVaults` (or REST equivalent), loop `agent-memory` + every `proj-*` vault per cycle, using the scoped agent key. Queue rows gain a `vault` column (sqlite migration: `ALTER TABLE ... ADD COLUMN vault TEXT DEFAULT 'agent-memory'`).
- Unchanged: LM Studio management, thinking-off guard, model benchmarks, exactly-once via digest flags.
- Note: with "remember goes to both", a deliberately-stored engram is enriched twice (once per vault). Accepted cost — deliberate remembers are a small fraction of volume, and each copy needs its own entity graph anyway since **entity graphs do not span vaults**.

### 5.4 New: `muninn-promoter`

Sibling of the enricher, sharing its config, LM Studio wrapper, sqlite-queue pattern, and thinking-off guard.

- **Candidate selection:** per project vault, engrams not yet assessed (no `promotion:assessed` tag), enriched (so entities/summary exist to judge from), older than a settling window (e.g. 24 h — recency bias hurts promotion judgement).
- **Assessment prompt (distinct from enrichment):** "Would this matter in a different project or in six months? Promote only durable, general knowledge: decisions with rationale, user preferences, reusable fixes/procedures, environment constraints. Never promote session narration, project-local file paths as facts, or transient state." Output: `{promote: bool, distilled_concept, distilled_content, type, tags}` — promotion **rewrites** the engram into its general form; it does not copy raw project text.
- **Write path:** promoted engrams go to `agent-memory` with tags `promoted`, `promoted:from:<vault>`, and `op:<sha8(distilled_content)>` for idempotency; source engram gains `promotion:assessed` (+`promotion:promoted` when applicable). Provenance = tags plus the **global entity registry** (§4.1 correction): shared entities link the promoted engram to its project-vault sources automatically.
- **Reuse and constraints (from docs audit):** near-duplicate clustering before promotion should reuse the LLM-assisted consolidation machinery (`plugins.md` §4 — merged engrams inherit union-of-associations, max stability, averaged confidence) instead of reinventing it; the promoter is built as a **retroactive processor** honouring the plugin layer's one non-negotiable rule — nothing ever touches the synchronous write path (no write-time mirroring). Contradiction detection is vault-local, so a promoted fact contradicting existing `agent-memory` content is only caught if the promoter runs `muninn_contradictions` against `agent-memory` after each batch — make that part of the run, not an afterthought.
- **Scheduling:** systemd user timer, daily; the run exits 0 immediately when the model isn't loaded (mirrors the enricher's intentional behaviour — **do not** treat model-absent restarts as failure). On-demand: `muninn-promoter --run [--vault X]`.

---

## 6. Recall budget

Defaults (all in `muninn-hook.json` `defaults`, overridable):

| Setting | Value |
|---|---|
| `project_weight` / `general_weight` | 0.67 / 0.33 |
| `prompt` limit (total, both tiers) | current limit unchanged |
| `orient` | project vault `where_left_off` + small general activate |
| floor | ≥1 result per tier when available (server-side, §4.2) |

---

## 7. Explicitly deferred

- **`agent-memory` rebuild** (clear + refill via a distillation-focused route): revisit only after project backfill is verified complete. Nothing in this design blocks or presumes it; the promoter gradually reshapes `agent-memory` either way.
- **Upstream PR** for §3/§4: after the fork feature has soaked in real use.
- **Parallel `ActivateMulti`**, promotion-quality feedback loops.
- **Archival/deletion tooling:** archival = `vault export` → delete (all vaults share one Pebble instance and one WAL — there is no per-project backup or encryption boundary, so export is the only per-vault extraction path; import-under-a-new-name doubles as vault rename). Deleting throwaway vaults must also clean the **global, non-ws-prefixed key spaces** (`0x0F` vault registry, `0x1F` entity registry, `0x23`) or orphaned entity rows accumulate — verify what `vault delete` currently covers before relying on it.

---

## 8. Implementation order & acceptance

Each phase lands and is verified independently; earlier phases never depend on later ones.

1. **Fork: multi-vault keys** (§3). Accept: full auth test matrix green; existing keys behave identically; re-issued `agent-memory,proj-*` keys pass a live smoke test against all current hook flows *before* any hook change ships.
2. **Fork: `vaults[]` recall + multi-vault remember** (§4). Accept: merge/scope/wire-compat tests green; manual two-vault recall returns labelled, sanely-ranked results.
3. **Hook: project resolution + provisioning + routing** (§5.1, behind a `"project_vaults": true` config flag for instant rollback). Accept: fresh session in this repo auto-creates its vault, stores land project-side, recall shows both tiers; a session in `~/` behaves exactly as today.
4. **Backfill v2** (§5.2), one pilot project first (this repo), then the fleet. Accept: pilot vault contains the repo's history; `op:` re-run is a no-op; spot-check that relaxed filtering captured detail the agent-memory backfill dropped.
5. **Enricher multi-vault** (§5.3). Accept: pending counts drain across vaults when the model is loaded.
6. **Promoter** (§5.4). Accept: dry-run (`--dry-run` prints assessments without writing) reviewed by Jake before the timer is enabled.

---

## 9. Risks

| Risk | Mitigation |
|---|---|
| Score merging across vaults misleads ranking | weighted RRF by rank, never raw-score normalisation (§4.2); same embed model everywhere, dimension guard surfaces violations |
| Entity registry is global → cross-vault probe surface | audit entity-read paths for scope filtering as part of §3's no-leak sweep |
| Key-format change is a one-way schema migration | Pebble backup before first scoped key; upstream-binary rollback impossible after |
| Cluster mode (if ever enabled) | single-writer Cortex runs all workers: promoter/enricher must run on Cortex only, epoch-fenced; dual writes double WAL replication volume |
| Entity graphs don't span vaults → weaker cross-project association | accepted by design; promoter re-materialises durable knowledge (with its own entities) in `agent-memory` |
| Wrong-vault writes (durable fact trapped in a project vault) | store policy in CLAUDE.md + promoter as safety net |
| Glob-scoped keys widen blast radius of a leaked agent key to all `proj-*` | keys remain local-only (127.0.0.1 server); grammar forbids bare `*` by default; `agent-memory` unchanged |
| Backfill duplication (transcripts × vault-copy × live) | single `op:<sha8>` contract everywhere + server dedup; transcript-primary rule (§5.2) |
| Hook regressions break all agents' memory at once | config flag per §8 phase 3; phases 1–2 verified before the hook changes at all |
