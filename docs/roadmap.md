# Roadmap

Single backlog, grouped by category and ordered by priority within each
group. See [CHANGELOG.md](../CHANGELOG.md) for what's already shipped, and
[docs/aws-production.md](aws-production.md) for the phase-2 AWS migration
that follows the On-Premises Rollout row group below.

| Category | Status | Priority | Task | Detail |
|---|---|---|---|---|
| Reliability & Resilience |  | **P3** | Postgres HA | Replication + automatic failover. Will be provided by the DB infrastructure team as a managed/hosted Postgres instance rather than built here — not this project's scope going forward |
| Testing |  | **P2** | Frontend unit tests | Add vitest + React Testing Library; cover `useTaskRunner`, `TaskForm`, `OutputPanel`, `HistoryPage`. No test framework is installed yet |
| Agent Intelligence |  | **P2** | Post-merge rework | Listen for GitHub review comments via webhooks, feed them back as a new task, agent pushes a fixup commit to the same branch |
| Agent Intelligence |  | **P2** | Merge conflict resolution | Detect conflicts on open PRs via webhook, trigger agent to rebase branch against main and force push. Deliberately not attempted yet — autonomous force-pushing is a design/safety decision that needs a human in the loop, not something to build unsupervised |
| Agent Intelligence |  | **P3** | Hard-block PR on missing security scan | `SecurityScanSkill` now surfaces a real error when checkov is missing, but `security_scan.md` still tells the agent to note it in the PR body and proceed to CreatePR anyway — nothing yet hard-blocks or requires explicit confirmation before a PR ships with no scan behind it |
| Observability |  | — | Repo-context index | `internal/tfscan` parses Terraform structure once per commit; `RepoScanSkill` checks a Postgres-backed cache before re-parsing. Genuinely saves **wall-clock/CPU** (benchmarked ~5x, see `docs/benchmarks.md`) — does **not** reduce LLM input tokens, since the same-size rendered summary is fed into the prompt on both a hit and a miss and each task is a fresh conversation with no cross-task memory to skip it from. A token-savings mechanism needs cross-task memory with real retrieval, which is scoped as the "Four-layer memory + retrieval" row below, not planned separately here |
| On-Premises Rollout |  | **P1** | Durable execution core | Postgres-backed leased task queue (retries, backoff, fencing tokens, dead-letter state) plus a run/attempt/step schema, so a killed worker resumes from the last valid step |
| On-Premises Rollout |  | **P1** | Replayable live state | Fully free the worker goroutine and NATS delivery during a pause for true cross-pod resume (needs the run/attempt/step checkpoint schema from the durable execution core item above) |
| On-Premises Rollout |  | **P2** | Mandatory sandboxing + real auth | [DONE] Make isolated container execution non-optional; [TODO] replace stored GitHub PATs with short-lived GitHub App installation tokens; replace long-lived AWS keys with IAM Roles Anywhere for Bedrock; add tenant/team/repo scoping across runs, memory, artifacts, and events |
| On-Premises Rollout |  | **P2** | Four-layer memory + retrieval | Working, episodic, repository, and organizational memory with source, scope, trust, expiry, and supersession; HCL-aware retrieval (exact + graph + lexical + vector) indexed by commit SHA |
| On-Premises Rollout |  | **P3** | Rollout validation | Versioned 50-100 task evaluation benchmark blocking regressions, plus load/chaos/security/restore testing and end-to-end observability (one run ID spanning API, worker, model, and PR) ahead of the pilot. Release gate: duplicate delivery, worker termination, API restart, and client reconnect must all survive within a single run with no state loss, no corrupted context, no dropped events, and no duplicate pull request |
