# Roadmap

Items are ordered by priority within each category. See [REVIEW.md](REVIEW.md) for the full resilience assessment.

---

## Reliability & Resilience

| Priority | Task | Detail |
|---|---|---|
| **P0** | NATS ACK timing fix | Move ACK to after task completion and Postgres persist — currently fires before task executes, risking message loss |
| **P0** | Stale task cleanup fix | Check `completed_at IS NOT NULL` before bulk-marking tasks failed on restart — prevents retroactively failing completed tasks |
| **P1** | Task idempotency | Add deduplication key to prevent the same task executing twice on queue redelivery |
| **P1** | Per-query DB retry | Exponential backoff on transient Postgres connection errors — currently a single blip fails the task |
| **P1** | Semaphore acquire timeout | Return 503 immediately if all LLM concurrency slots are full — currently blocks indefinitely |
| **P2** | Graceful shutdown drain | Wait for in-flight tasks to complete before exiting — currently tasks are killed after the 30s HTTP drain window |
| **P2** | LLM circuit breaker | Fast-fail new requests when the LLM provider is degraded instead of queuing them |
| **P3** | Encryption key versioning | Store key ID alongside ciphertext to support key rotation without re-encrypting all tokens manually |
| **P3** | Postgres HA | Replication + automatic failover — Postgres is currently a single point of failure |
| **P3** | Per-user semaphore cleanup | Semaphore map grows unboundedly — clean up entries for inactive users |

---

## Testing

| Priority | Task | Detail |
|---|---|---|
| **P1** | E2E smoke test | One end-to-end test (HTTP-level) that submits a task and verifies SSE output is produced |
| **P1** | Skill integration tests | Unit tests for Validate, SecurityScan, DriftDetect with mock `exec.Command` to cover error paths |
| **P2** | Frontend unit tests | Add vitest + React Testing Library; cover `useTaskRunner`, `TaskForm`, `OutputPanel`, `HistoryPage` |

---

## Agent Intelligence

| Priority | Task | Detail |
|---|---|---|
| **P1** | Validate auto-fix loop | Parse `terraform validate -json` output, feed errors back into the agent loop for one retry before failing — currently the skill reports errors but does not attempt repair |
| **P2** | Post-merge rework | Listen for GitHub review comments via webhooks, feed them back as a new task, agent pushes a fixup commit to the same branch |
| **P2** | Merge conflict resolution | Detect conflicts on open PRs via webhook, trigger agent to rebase branch against main and force push |

---

## Observability

| Priority | Task | Detail |
|---|---|---|
| **P1** | Enhanced `/healthz` | Add queue depth, LLM concurrency saturation, and task error rate to health check — currently only checks DB and encryption key |
| **P2** | Tool-level metrics | Per-tool execution duration and success/failure rate at `/metrics` |
| **P2** | Queue depth metric | Expose queue depth and wait time per named queue |
| **P3** | Cache hit/miss metrics | Emit prompt cache hit/miss as Prometheus counters (data already available from SSE events) |

---

## Performance

| Priority | Task | Detail |
|---|---|---|
| **P2** | Prompt caching | Apply `cache_control` markers to system prompt blocks — can cut latency 30–60% on long tasks |

---

## Deployment & Operations

| Priority | Task | Detail |
|---|---|---|
| **P1** | Task output pagination | Add `GET /v1/tasks/{id}/output?offset=N&limit=N` — large outputs currently returned in one unbounded response |
| **P2** | Terraform execution sandbox | Run `terraform`, `tflint`, and `checkov` inside a Docker container with a volume-mounted working directory — required for multi-tenant deployments |
| **P2** | Toolchain health check | Extend `make doctor` to verify `terraform`, `tflint`, and `checkov` are installed and print install instructions when missing |
