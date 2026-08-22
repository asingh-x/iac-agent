# iac-agent Architecture

## What this is

A single Go binary (`cmd/server`) that accepts a task (a prompt, or a structured
input) over HTTP, runs it through an LLM-driven tool-calling loop
(`internal/agent`), and lets that loop call a fixed set of tools
(`internal/tools`: Read, Write, Edit, Glob, Grep, Ls, Bash, Agent, AskUser,
Task, WebFetch, WebSearch) against a real filesystem/repo. Output is either
printed text or a pull request. A React SPA (`client/`) is embedded into the
binary and served from the same process.

## Request flow

1. `POST /v1/tasks` (`internal/server/handler.go: handleSubmitTask`) validates
   input, creates a `tasks` row (`internal/db`), and pushes the task ID onto a
   queue (`internal/queue`).
2. `internal/server/task_runner.go: Runner` pulls tasks off the queue (respecting
   `llm_concurrency` and `per_user_concurrency` from config), decrypts the
   user's GitHub/Atlassian tokens (`internal/server/crypto.go`), and hands off
   to `internal/agent`.
3. `internal/agent` runs the LLM tool-calling loop (`loop.go`), building the
   system prompt (`prompt.go`) and compacting history when it grows too long
   (`compact.go`). Each tool call is checked against `internal/permissions`
   before it runs (config-driven: `auto` / `ask` / `deny` per tool, see
   `config.sample.toml`'s `[permissions]` block).
4. Tool results and task state stream back to the client over SSE
   (`internal/server/stream.go`, `Hub`), and are persisted incrementally to the
   `tasks` row (`output`, `input_tokens`, `output_tokens`, `status`).
5. `internal/skills` provides higher-level, multi-step flows (RepoScan,
   Clarifier, Generate, Validate, SecurityScan, CreatePR, JiraFetch,
   DriftDetect) that the agent loop can invoke as compound tools.

## Data

PostgreSQL is the only supported store (`internal/db/postgres.go`); an
in-memory implementation (`internal/db/memory.go`) exists solely for unit
tests and is never used in production. Schema changes are versioned SQL files
under `internal/db/migrations/`, applied automatically and idempotently on
every process start (`internal/db/migrate.go`) — see that package for the
exact mechanism. `internal/queue` is in-memory by default; a NATS-backed queue
is available for multi-instance deployments (`make infra` starts NATS +
Postgres locally).

## Security boundaries

- Every `/v1/*` route requires `Authorization: Bearer <token>`
  (`internal/server/auth.go: authMiddleware`); `/v1/admin/*` routes
  additionally require `adminMiddleware`.
- Tokens are `tfa-` + 40 hex chars; only a SHA-256 hash is stored, the raw
  value is shown once at creation/regeneration time.
- GitHub/Atlassian tokens in `user_settings` are AES-256-GCM encrypted at
  rest and decrypted only inside `task_runner.go` immediately before use.
- Every admin mutation on a user (create, update, delete, activate/deactivate,
  token regenerate/revoke) writes an `audit_events` row recording the acting
  admin, the action, and the target — queryable via
  `GET /v1/admin/audit-log`. Self-action guards prevent an admin from
  deleting, deactivating, or revoking their own account/token.
- `internal/tools` must never import `internal/agent` (import-cycle
  prevention) — cross-package calls go through injected function types (e.g.
  `SubAgentRunner`) wired in `internal/server/task_runner.go`.

## Running locally

See `CLAUDE.md` for the up-to-date command list (`make build`, `make dev`,
`make infra`, `make doctor`). This document explains *why* the pieces are
shaped this way; `CLAUDE.md` is the quick-reference for *how* to build and run
them day to day — if the two disagree, trust `CLAUDE.md` for commands and file
an issue to fix this doc.
