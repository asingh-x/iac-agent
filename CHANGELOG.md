# Changelog

All notable changes to iac-agent are documented here.

## [1.0.0] — 2026-09-03

**Multi-replica safety** — the server can now run as N Kubernetes pod
replicas behind a plain load balancer, no sticky sessions. SSE streaming and
answer/permission/cancel requests are relayed cross-pod over NATS
(`internal/server/event_relay.go`, `control_relay.go`) whenever
`queue_driver=nats` is set. A final whole-branch review caught two Critical
bugs in the initial implementation before merge — an unbounded relay-request
timeout that could hang forever, and an SSE fallback that could lose the
first ~10s of a cross-pod stream or hang permanently — both fixed and
verified live against a real NATS server. Building the two-pod end-to-end
test suite this required also surfaced and fixed two more real bugs: a local
SSE channel existing doesn't actually mean a pod owns the task (`Hub.Claim`
now tracks real ownership), and a pod's *graceful* shutdown could delete the
shared NATS work-queue consumer out from under every other pod
(`NATSQueue.Close()` no longer unsubscribes).

**Backend hardening**: real permission-pause flow (tasks genuinely wait for
approve/deny instead of auto-approving), an LLM provider circuit breaker,
path-scoping on file tools, a repo-index cache to avoid re-parsing an
unchanged repo every task (saves CPU/wall-clock, not LLM tokens — see
`docs/benchmarks.md`), graceful shutdown drain with a real `-race`-caught
fix, per-query Postgres retry on transient connection errors, a bounded LLM
concurrency semaphore (previously could block forever), per-user semaphore
cleanup, an age-based reconciliation backstop for tasks that never reach a
terminal status, and `/v1/tasks/{id}/output` pagination.

**Observability**: `/healthz` now reports LLM concurrency saturation and a
task error-rate window; prompt cache hit/miss and per-tool execution metrics
added at `/metrics`; benchmarks for the cross-pod relay and repo-index cache.

**Terraform execution sandbox**: `terraform`/`tflint`/`checkov` now always
run inside a locked-down Docker container (`--network=none`, dropped
capabilities, non-root, resource limits) or Kubernetes `Job` (configurable
via `sandbox_backend`) instead of directly on the host. A Kubernetes
`Job`-based executor for multi-node deployments is verified for real against
a live cluster (Job/pod lifecycle, file staging, guaranteed cleanup, a
`NetworkPolicy` confirmed actually enforced) — see `docs/sandbox.md`. The
sandbox image itself is published, multi-arch (amd64+arm64), to
`ghcr.io/asingh-x/iac-agent/sandbox:latest` — the default for
`sandbox_image`, so both backends work out of the box with no local build
required. The `sandbox_enabled` toggle is removed; sandboxing is now
mandatory (renamed from a leftover `tf-agent-sandbox` name along the way).

**Agent loop**: `ValidateSkill` now parses `terraform validate -json` /
`tflint --format=json` into structured, actionable diagnostics instead of
dumping raw JSON — also fixed a bug where a non-zero exit code (the normal
outcome when either tool finds real issues) discarded valid findings
entirely in favor of a useless error message.

**GitHub PR creation**: `CreatePRSkill` is now idempotent per task — the
server derives a deterministic `iac-agent/task-<TaskID>` branch name instead
of trusting the LLM-suggested one, and checks branch/PR existence on GitHub
before creating either, so a redelivered or retried task resumes from
wherever it left off (including a straight-to-existing-PR-URL return)
instead of failing or opening a duplicate PR.

**Replayable live state**: SSE events are now persisted to an ordered
`run_events` table, so a client reconnecting with `Last-Event-ID` replays
everything it missed instead of losing it. The LLM-concurrency semaphore is
also now released while a task is paused on a question or permission prompt
(previously held for up to 7 days) via a `pauseGate` depth-counter that
safely handles overlapping pauses on the same task. A final review caught
and fixed a reproduced hang (reconnecting right as a task finishes could
leave the SSE connection stuck forever), a related per-user semaphore-sweeper
race, and an unbounded DB write on the event-publish hot path.

**Durable execution core**: a Postgres-backed leased task queue
(`queue_driver = "postgres"`) as an alternative to NATS, for on-prem
deployments that don't want a second piece of queue infrastructure. Atomic
claim via `SELECT ... FOR UPDATE SKIP LOCKED`, fencing-token-safe
Ack/Extend/Nak (a lease-expired "zombie" worker can never corrupt a row a
reclaiming worker now owns), exponential backoff, and dead-letter tracking.
A final whole-branch review caught two Critical bugs before merge: decrypted
GitHub/Atlassian tokens were being retained in cleartext forever (`Ack` now
deletes the row instead of soft-marking it done, and the dead-letter path
strips credentials from the payload), and any Postgres error during polling
caused an unthrottled hot-spin against an already-unhealthy database
(`Pop` now backs off and logs instead of returning). Known, tracked, not
fixed here: the dead-letter cap only applies via the explicit `Nak()` path,
not a worker that crashes on every attempt without ever calling it; and a
pre-existing, driver-agnostic bug where graceful shutdown's drain goroutine
can be killed mid-flight by `main()` exiting before it finishes — see
`docs/roadmap.md`'s Known issues.

**Sandboxing hardening**: `ValidateSkill`/`SecurityScanSkill`'s sandbox
executor construction is now unconditional — no more nil-executor fallback
to a direct host `exec` call, closing the last opt-out path.

**Encrypted secrets with key rotation**: ciphertexts now carry a key ID
(`<keyID>:<ciphertext>`), so a compromised or aging encryption key can be
rotated (moved to `TF_AGENT_ENCRYPTION_KEYS_OLD`, decrypt-only) without a
bulk re-encryption job — existing unprefixed ciphertexts keep decrypting
against the current key with no migration needed. A final review caught a
Critical bug before merge: the legacy (unprefixed) decrypt path never fell
back to old keys, so the very first rotation would have permanently orphaned
every token not yet re-saved — fixed and re-verified. See
`docs/configuration.md` for the rotation procedure.

**Known-issue fixes**: `NATSQueue.Len()` now reports the calling queue's own
pending count instead of the whole shared stream's total (two named queues
no longer report an identical, inflated depth); `SecurityScanSkill` now
returns a real error when `checkov` is missing instead of a silent
non-error "skipping" message that read identically to a completed scan.

**Frontend test coverage**: vitest + React Testing Library added to the
client (previously zero test infrastructure) — real coverage for
`useTaskRunner`, `TaskForm`, `OutputPanel`, and `HistoryPage`. Runs via
`make test` alongside the Go suite.

See `docs/roadmap.md` for what's still open.

## Initial — 2026-04-03

Initial release — autonomous Terraform agent. Takes a prompt or Jira ticket, runs an 8-skill pipeline, and opens a validated GitHub PR with no human in the loop.

