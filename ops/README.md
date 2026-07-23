# ops — MuninnDB cognitive-memory operations stack

Deployment/worker scripts that drive this MuninnDB fork as a personal
cognitive-memory system for coding agents (Claude Code, Codex, pi). These are
the **canonical sources** for tooling that otherwise lives loose in
`~/.local/bin`; vendored here for version history and backup.

They are self-contained Python 3 (stdlib only, no third-party deps) and talk to
the server over its REST API (`internal/transport/rest`) and MCP endpoint. All
secrets are loaded from `~/.config/muninn-hook.json` — see the sanitized
`muninn-hook.json.template` and fill in your own scoped `mk_` keys. **No live
keys are committed here**; any `mk_REDACTED_...` placeholder must be replaced
locally.

## Architecture

Two-tier memory: `agent-memory` (durable, cross-project) + per-project
`proj-<slug>-<hash8>` vaults (auto-provisioned per git root, or mapped via the
registry in the config). Raw session turns route to their project vault (or
`proj-general` when the cwd isn't a project); deliberate remembers target
whichever vault(s) the agent chooses.

## Scripts (`ops/bin/`)

| Script | Role |
|--------|------|
| `muninn-hook` | The integration hook wired into agent lifecycle events (session-start / per-prompt / post-tool-error recall; turn / compaction / subagent auto-store). Routes stores/recalls per cwd; two-tier recall. |
| `muninn-enricher` | Deferred worker: pulls engrams missing enrichment stages, extracts entities/relationships/summary via an LM Studio model, persists via `muninn_apply_enrichment`. Sequential (1 stream); model-absent → exit 0. |
| `muninn-promoter` | Deferred worker: distils settled project-vault engrams up into `agent-memory` (tagged `promoted`). Evolves a prior promotion only on an exact op-hash match — never on semantic similarity. |
| `muninn-contradictor` | Deferred worker: finds contradictory engram pairs (shared entities + semantic similarity), LLM-judges them, writes `contradicts` links. |
| `muninn-distributor` | One-shot: routes deliberate `agent-memory` remembers into their originating project vaults (LLM-classified), tagging `distributed:to:<vault>` / `distributed:general`. |
| `muninn-backfill` | Ingests historical agent session transcripts (`pi` / `claude` / `codex`) into the right vault per session cwd. |
| `muninn-pending-sweep` | Finds engrams still carrying `enrichment:pending` and enqueues them for the enricher. |
| `muninn-agentmem-cleanse` | Prunes raw-turn / distributed noise from `agent-memory` so it becomes a curated tier, preserving deliberate remembers + promotions. Snapshot + soft-delete (restorable). |
| `muninn-promoter-recover` | Recovery tool for a promoter twin-collision incident: restores overwritten originals and re-inserts promotions cleanly. |
| `muninn-promoter-loop` | Runs the promoter continuously to clear a large backlog. |
| `muninn-phd-add` | Registers a new project repo (registry + recall group) and backfills it. |
| `muninn-status` | Prints enricher queue status. |

## Config

Copy `muninn-hook.json.template` to `~/.config/muninn-hook.json`, replace the
`mk_REDACTED_...` placeholders with your scoped API keys (scope
`agent-memory,proj-*`), and adjust `projects` / `recall_groups` to your paths.
The enricher/promoter/contradictor/distributor share a separate
`~/.config/muninn-enricher.json` (endpoint, LM Studio connection, per-worker
model + reasoning settings) — not templated here as it's environment-specific.

## Server deploy

Build with the version stamp and install to both binary names, then restart the
systemd user unit:

```
go build -tags localassets -ldflags "-X main.version=$(git describe --tags)" -o muninndb-server ./cmd/muninn/...
install -m755 muninndb-server ~/.local/bin/muninn.real
install -m755 muninndb-server ~/.local/bin/muninn
systemctl --user restart muninndb.service
```
