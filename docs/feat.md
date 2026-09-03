# Features

A persistent reference of every high-level capability the agent has, so
shipped work doesn't get lost across many small changes. See
[CHANGELOG.md](../CHANGELOG.md) for release history and
[docs/roadmap.md](roadmap.md) for what's still open. Update this file
whenever a feature ships — a new row here, not a mental note.

| Category | Capability | Detail |
|---|---|---|
| Agent pipeline | `RepoScan` | Scans reference repos for naming conventions and module patterns; caches the parsed index per commit SHA (~5x faster on a cache hit) |
| Agent pipeline | `Clarifier` | Asks targeted follow-up questions before writing any code |
| Agent pipeline | `Generate` | Writes Terraform HCL matched to your repo's conventions |
| Agent pipeline | `Validate` | Runs `terraform validate` + `tflint`, parses output into structured diagnostics (file:line + message, not raw JSON) |
| Agent pipeline | `SecurityScan` | Runs `checkov` static analysis, surfaces policy violations |
| Agent pipeline | `CreatePR` | Opens a GitHub pull request with the generated code — idempotent per task (deterministic branch name derived from the task ID, checks GitHub for an existing branch/PR before creating anything, so a redelivered or retried task resumes instead of duplicating) |
| Agent pipeline | `JiraFetch` | Reads a Jira ticket and uses it as the task specification |
| Agent pipeline | `DriftDetect` | Compares live infrastructure state against Terraform code via `terraform plan` |
| Agent behavior | Mid-task questions | Agent pauses, asks a clarifying question via `ask_user`, waits up to 7 days for an answer (configurable) |
| Agent behavior | Tool permission gating | `auto` / `confirm` / `deny` policy per tool; destructive tools pause for human approval |
| Agent behavior | Concurrency-safe pauses | While paused (question or permission prompt), the task releases its LLM-concurrency slot instead of holding it — a `pauseGate` depth-counter keeps this safe even when two pauses overlap on the same task |
| Agent behavior | Sub-agents | Spawns isolated reviewer / coder / tester / security-auditor sub-agents for focused tasks |
| Agent behavior | Live streaming | Every step streamed to the client via SSE in real time — no black box |
| Agent behavior | Reconnect replay | SSE events are persisted to an ordered `run_events` table; a client reconnecting with `Last-Event-ID` replays everything it missed instead of losing it |
| Agent behavior | Prompt caching | Avoids re-sending large repeated context on every LLM call |
| Agent behavior | Multi-provider LLM | Anthropic API or AWS Bedrock — swap in config, no code change |
| Agent behavior | Provider circuit breaker | Trips on repeated LLM provider failures instead of hammering a downed endpoint |
| Execution & safety | Sandboxed execution | Mandatory — `terraform`/`tflint`/`checkov` always run inside a Docker or Kubernetes sandbox, network-isolated, non-root, resource-limited; no host-exec fallback (see [docs/sandbox.md](sandbox.md)) |
| Execution & safety | Path-scoped file tools | Read/Write/Edit/Glob/Grep are scoped to the task's working directory |
| Execution & safety | Encrypted secrets | GitHub and Atlassian tokens stored AES-256-GCM encrypted at rest, with versioned keys — a key ID travels with each ciphertext so keys can be rotated (old key kept decrypt-only) without a bulk re-encryption job (see [docs/configuration.md](configuration.md)) |
| Reliability & scale | Multi-replica safe (NATS driver) | Run N pods behind a plain load balancer, no sticky sessions — SSE streams and answer/permission/cancel requests work regardless of which pod a request lands on, via a NATS cross-pod relay (see [docs/architecture.md](architecture.md)). The `postgres` queue driver below does not yet have this cross-pod relay — durable but single-replica for the control plane |
| Reliability & scale | Durable execution core | Postgres-backed leased task queue (`queue_driver = "postgres"`) as an alternative to NATS: atomic claim via `SELECT ... FOR UPDATE SKIP LOCKED`, fencing-token-safe Ack/Extend/Nak, exponential backoff, and dead-letter tracking — no separate queue infrastructure needed since it reuses the main Postgres database (see [docs/configuration.md](configuration.md)) |
| Reliability & scale | Bounded LLM concurrency | Global + per-user semaphores, pause-aware (see "Concurrency-safe pauses" above) so a stalled task can't starve the pool |
| Reliability & scale | Graceful shutdown drain | In-flight tasks finish before the process exits |
| Reliability & scale | Stale-task reconciliation | Age-based backstop marks tasks failed if they never reach a terminal state, without falsely failing tasks legitimately paused on human input |
| Reliability & scale | Repo-index cache | Postgres-backed cache of parsed Terraform structure, invalidated per commit SHA (see [docs/benchmarks.md](benchmarks.md)) |
| Testing | Frontend unit tests | vitest + React Testing Library coverage for `useTaskRunner`, `TaskForm`, `OutputPanel`, `HistoryPage` — runs via `make test` alongside the Go suite |
| Multi-tenant & admin | Multi-user | Admin/member roles, per-user API keys (`tfa-` format), token revocation |
| Multi-tenant & admin | Audit trail | Every admin action (user create/update/delete/activate/token regen) logged with actor, action, and target — queryable at `/v1/admin/audit-log` |
| Observability | Prometheus metrics | Task duration, token usage, throughput, prompt cache hit/miss, per-tool execution counts at `/metrics` |
| Observability | Health reporting | `/healthz` reports LLM concurrency saturation and a task error-rate window |
| Observability | Benchmarks | Cross-pod relay latency and repo-index cache benefit, with real numbers (see [docs/benchmarks.md](benchmarks.md)) |
