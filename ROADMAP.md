# Roadmap

Items are ordered by priority within each category. See [REVIEW.md](REVIEW.md) for the full resilience assessment.

---

## Reliability & Resilience

| Status | Priority | Task | Detail |
|---|---|---|---|
| ✅ Done | **P0** | NATS ACK timing fix | ACK now fires only after the task's result is durably persisted; a failed persist NAKs instead, so the queue redelivers rather than losing the task |
| ✅ Done | **P0** | Stale task cleanup fix | Added `AND completed_at IS NULL` to the bulk stale-task sweep — defense in depth against retroactively failing a task that already completed |
| ✅ Done | **P1** | Task idempotency | A redelivered message for a task already in a terminal state is recognized and skipped (acked, not re-run) instead of executing twice |
| ⬜ Not done | **P1** | Per-query DB retry | Exponential backoff on transient Postgres connection errors — currently a single blip fails the task |
| ⬜ Not done | **P1** | Semaphore acquire timeout | Return 503 immediately if all LLM concurrency slots are full — currently blocks indefinitely |
| ✅ Done | **P2** | Graceful shutdown drain | In-flight tasks now drain within a configurable grace period (`shutdown_grace_period`, default 60s) instead of being killed immediately on signal |
| ✅ Done | **P2** | LLM circuit breaker | Wraps Anthropic/Bedrock providers; opens after 5 consecutive failures (sync **or** async — i.e. an `EventError` mid-stream, not just a hard `Stream()` error), 30s cooldown, half-open trial to recover |
| ⬜ Not done | **P3** | Encryption key versioning | Store key ID alongside ciphertext to support key rotation without re-encrypting all tokens manually |
| ⬜ Not done | **P3** | Postgres HA | Replication + automatic failover — Postgres is currently a single point of failure |
| ⬜ Not done | **P3** | Per-user semaphore cleanup | Semaphore map grows unboundedly — clean up entries for inactive users |
| ✅ Done *(new)* | — | NATS publish backpressure | `Push` now rejects once the shared stream hits a configurable message cap (`nats_max_msgs`, default 5000) instead of accepting unbounded work. Also fixed a real pre-existing bug found along the way: an inverted condition meant `UpdateStream` never actually applied config changes to an already-existing stream |
| ✅ Done *(new)* | — | Path-scoping on file tools | write/edit/ls/glob/grep now reject any path (absolute or `..`-relative) that escapes the task's working directory — previously only `read` had a (weaker) check |
| ✅ Done *(new)* | — | Destructive-tool confirmation, for real | bash/write/edit require confirmation by default, **and** the server actually pauses and waits for a human decision now — previously the permission event existed but the server auto-approved every request unconditionally. Verified live end-to-end (approve and deny) against the real Anthropic API |
| ✅ Done *(new)* | — | Data race in graceful shutdown | `go test -race` caught a real `sync.WaitGroup.Add`-vs-`Wait` race between task dispatch and `Shutdown`; fixed with a `draining` flag synchronized under a `RWMutex`. Regression test added |

---

## Testing

| Status | Priority | Task | Detail |
|---|---|---|---|
| ✅ Done (pre-existing) | **P1** | E2E smoke test | One end-to-end test (HTTP-level) that submits a task and verifies SSE output is produced |
| 🟡 Partial | **P1** | Skill integration tests | Not the originally-scoped Validate/SecurityScan/DriftDetect mock tests, but new integration coverage was added for the repo-index cache and the full permission approve/deny flow (real HTTP + SSE, not mocks) |
| ⬜ Not done | **P2** | Frontend unit tests | Add vitest + React Testing Library; cover `useTaskRunner`, `TaskForm`, `OutputPanel`, `HistoryPage`. No test framework is installed yet |

---

## Agent Intelligence

| Status | Priority | Task | Detail |
|---|---|---|---|
| ⬜ Not done | **P1** | Validate auto-fix loop | Parse `terraform validate -json` output, feed errors back into the agent loop for one retry before failing — currently the skill reports errors but does not attempt repair |
| ⬜ Not done | **P2** | Post-merge rework | Listen for GitHub review comments via webhooks, feed them back as a new task, agent pushes a fixup commit to the same branch |
| ⬜ Not done | **P2** | Merge conflict resolution | Detect conflicts on open PRs via webhook, trigger agent to rebase branch against main and force push |

---

## Observability

| Status | Priority | Task | Detail |
|---|---|---|---|
| 🟡 Partial | **P1** | Enhanced `/healthz` | Queue depth added. LLM concurrency saturation and task error rate still missing (queue-depth reporting itself has a known bug — see below) |
| ⬜ Not done | **P2** | Tool-level metrics | Per-tool execution duration and success/failure rate at `/metrics` |
| ✅ Done | **P2** | Queue depth metric | Exposed via `/healthz` and a `tfagent_queue_depth` Prometheus gauge. **Known bug**: `NATSQueue.Len()` returns the total message count across the whole shared stream, not the count for the specific named queue/subject — cosmetic/observability only, not a safety issue, but will misreport once more than one named queue is in use |
| 🟡 Partial | **P3** | Cache hit/miss metrics | Not the prompt-cache metric originally scoped here — a *different* cache got instrumented instead: `tfagent_repo_index_cache_total{result}` for the new repo-context index (see below). Prompt cache hit/miss (from `CacheRead`/`CacheCreated`) is still unmetered |
| ✅ Done *(new)* | — | Repo-context index (the token-savings engine) | New `internal/tfscan` package parses Terraform structure (resources/modules/variables/outputs) once per commit; `RepoScanSkill` checks a Postgres-backed cache (`repo_index` table, migration `0004`) keyed on `(repo, commit sha)` before re-parsing. This is what actually stops re-comprehending an unchanged repo every task |

---

## Performance

| Status | Priority | Task | Detail |
|---|---|---|---|
| ✅ Done | **P2** | Prompt caching | `cache_control` markers now applied to the system prompt block and the last block of the growing conversation history, so repeated turns/tasks against the same system prompt actually hit Anthropic's prompt cache (the token-accounting plumbing existed before, but caching could never activate) |

---

## Deployment & Operations

| Status | Priority | Task | Detail |
|---|---|---|---|
| ⬜ Not done | **P1** | Task output pagination | Add `GET /v1/tasks/{id}/output?offset=N&limit=N` — large outputs currently returned in one unbounded response |
| ⬜ Not done, flagged as accepted risk | **P2** | Terraform execution sandbox | Run `terraform`, `tflint`, and `checkov` inside a Docker container with a volume-mounted working directory — required for multi-tenant deployments. This is the single biggest remaining security gap before exposing this to many real users; do not present it as done |
| ⬜ Not done | **P2** | Toolchain health check | Extend `make doctor` to verify `terraform`, `tflint`, and `checkov` are installed and print install instructions when missing |

---

## Known issues surfaced tonight (not yet fixed)

- **Queue-depth metric bug** (see Observability above) — `NATSQueue.Len()` counts the whole shared stream, not the calling queue's own subject.
- **`SecurityScanSkill` silently skips instead of failing loudly** when `checkov` isn't on `PATH` — returns a normal (non-error) "skipping" message, so the agent can (and in one observed run, did) proceed and assert security properties are "enforced directly in the generated code" without that claim being backed by an actual scan. Worth hardening: either fail the task, or require explicit confirmation before a PR goes out with no security scan behind it.
