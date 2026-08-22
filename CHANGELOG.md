# Changelog

All notable changes to iac-agent are documented here.

## [Unreleased] — 2026-08-22

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

**Terraform execution sandbox**: `terraform`/`tflint`/`checkov` can now run
inside a locked-down Docker container (`--network=none`, dropped
capabilities, non-root, resource limits) instead of directly on the host —
opt-in via `sandbox_enabled`, off by default. A Kubernetes `Job`-based
executor for multi-node deployments is also built and verified for real
against a live cluster (Job/pod lifecycle, file staging, guaranteed cleanup,
a `NetworkPolicy` confirmed actually enforced) — see `docs/sandbox.md`. The
sandbox image itself is now published, multi-arch (amd64+arm64), to
`ghcr.io/asingh-x/iac-agent/sandbox:latest` — the new default for
`sandbox_image`, so both backends work out of the box with no local build
required (renamed from a leftover `tf-agent-sandbox` name along the way).

**Agent loop**: `ValidateSkill` now parses `terraform validate -json` /
`tflint --format=json` into structured, actionable diagnostics instead of
dumping raw JSON — also fixed a bug where a non-zero exit code (the normal
outcome when either tool finds real issues) discarded valid findings
entirely in favor of a useless error message.

See `docs/roadmap.md` for what's still open.

## [0.1.0] — 2026-04-03

Initial release — autonomous Terraform agent. Takes a prompt or Jira ticket, runs an 8-skill pipeline, and opens a validated GitHub PR with no human in the loop.

