# Roadmap

Items are ordered by priority within each category. See [CHANGELOG.md](../CHANGELOG.md) for what's already shipped.

---

## Reliability & Resilience

| Status | Priority | Task | Detail |
|---|---|---|---|
|  | **P3** | Encryption key versioning | Store key ID alongside ciphertext to support key rotation without re-encrypting all tokens manually |
|  | **P3** | Postgres HA | Replication + automatic failover. Will be provided by the DB infrastructure team as a managed/hosted Postgres instance rather than built here — not this project's scope going forward |

---

## Testing

| Status | Priority | Task | Detail |
|---|---|---|---|
|  | **P1** | Skill integration tests | Not the originally-scoped Validate/SecurityScan/DriftDetect mock tests, but new integration coverage was added for the repo-index cache and the full permission approve/deny flow (real HTTP + SSE, not mocks) |
|  | **P2** | Frontend unit tests | Add vitest + React Testing Library; cover `useTaskRunner`, `TaskForm`, `OutputPanel`, `HistoryPage`. No test framework is installed yet |

---

## Agent Intelligence

| Status | Priority | Task | Detail |
|---|---|---|---|
|  | **P2** | Post-merge rework | Listen for GitHub review comments via webhooks, feed them back as a new task, agent pushes a fixup commit to the same branch |
|  | **P2** | Merge conflict resolution | Detect conflicts on open PRs via webhook, trigger agent to rebase branch against main and force push. Deliberately not attempted yet — autonomous force-pushing is a design/safety decision that needs a human in the loop, not something to build unsupervised |

---

## Observability

| Status | Priority | Task | Detail |
|---|---|---|---|
|  | — | Repo-context index | `internal/tfscan` parses Terraform structure once per commit; `RepoScanSkill` checks a Postgres-backed cache before re-parsing. Genuinely saves **wall-clock/CPU** (benchmarked ~5x, see `docs/benchmarks.md`) — does **not** reduce LLM input tokens, since the same-size rendered summary is fed into the prompt on both a hit and a miss and each task is a fresh conversation with no cross-task memory to skip it from. An actual token-savings mechanism would need a different design and isn't currently planned |

---

## Known issues (not yet fixed)

- **Queue-depth metric bug** — `NATSQueue.Len()` counts the whole shared stream, not the calling queue's own subject. Cosmetic/observability only, not a safety issue, but will misreport once more than one named queue is in use.
- **`SecurityScanSkill` silently skips instead of failing loudly** when `checkov` isn't on `PATH` — returns a normal (non-error) "skipping" message, so the agent can (and in one observed run, did) proceed and assert security properties are "enforced directly in the generated code" without that claim being backed by an actual scan. Worth hardening: either fail the task, or require explicit confirmation before a PR goes out with no security scan behind it.
