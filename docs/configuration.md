# Configuration

iac-agent is configured via a TOML file at `~/.tf-agent/config.toml`. Copy the sample and edit:

```bash
cp config.sample.toml ~/.tf-agent/config.toml
```

```toml
[server]
port = 8080
llm_concurrency = 10

[provider]
name  = "anthropic"           # anthropic | bedrock
model = "claude-opus-4-6"

[provider.anthropic]
api_key = ""                  # leave empty — use ANTHROPIC_API_KEY env var instead

[permissions]
default = "auto"              # auto | confirm | deny — fallback for skills/tools not listed individually
bash    = "confirm"           # destructive tools (bash/write/edit) default to confirm; everything else defaults to auto
write   = "confirm"
edit    = "confirm"

[agent]
wait_for_input_timeout = 604800  # seconds before a paused task times out (default: 7 days)
```

## Env vars vs config file

| Scenario | Recommendation |
|---|---|
| Local dev | Use `~/.tf-agent/config.toml` for everything except the API key |
| CI / Docker | Use environment variables only — no file on disk |
| Production server | Config file for stable settings; env vars for secrets (`ANTHROPIC_API_KEY`, `DB_URL`) |

**Secrets should never be in the config file in production.** Pass them as env vars:

```bash
ANTHROPIC_API_KEY=sk-ant-...
DB_URL=postgres://user:pass@host:5432/db?sslmode=disable
TF_AGENT_ADMIN_TOKEN=tfa-...
```

Environment variables always override values in `config.toml`.

Per-user GitHub and Atlassian tokens can be saved via the Settings page. They are stored AES-256-GCM encrypted at rest.

## Infrastructure setup

iac-agent ships with one-command Docker infra bootstrap — no docker-compose needed.

```bash
# Start Postgres 16 + NATS 2.10 (JetStream) containers
make infra

# Check status
make infra-status

# Run server with Postgres + NATS
DB_DRIVER=postgres QUEUE_DRIVER=nats make run-server

# Stop containers (data preserved)
make infra-stop

# Remove containers + volumes (destructive)
make infra-clean
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `DB_URL` | — | **Required.** Postgres DSN: `postgres://user:pass@host:5432/db?sslmode=disable` |
| `QUEUE_DRIVER` | `memory` | `memory`, `nats`, or `postgres` |
| `NATS_URL` | `nats://127.0.0.1:4222` | NATS server URL (used when `QUEUE_DRIVER=nats`) |
| `QUEUE_NAMES` | `default` | Comma-separated named queues — each gets its own worker goroutine (e.g. `default,security`) |
| `TF_AGENT_ADMIN_TOKEN` | — | Bootstrap admin token on first run |

### Queue driver selection

Three queue drivers are available:

- **`memory`** (default): In-memory queue; tasks are lost on restart. Only suitable for development and single-instance deployments.
- **`nats`**: Durable, clustered NATS JetStream queue. Requires a separate NATS infrastructure (see `NATS_URL`).
- **`postgres`**: Durable leased task queue backed by Postgres. No separate infrastructure needed — Postgres, which you already require for state storage, also owns the queue. Task leases, retries with backoff, and dead-letter tracking are all handled via the `task_queue` table. **Pick this for on-premises deployments** where you want durable execution without adding a new service dependency. **Caveat:** unlike `nats`, this driver has no cross-pod control plane relay wired up — the queue itself is safely durable and shared across replicas, but answer/permission/cancel requests AND live SSE output streaming for a running task will fail or hang if load-balanced to a pod that doesn't own it. Fine for a single replica; for multiple replicas either route control-plane requests to the owning pod (sticky routing) or use `queue_driver = "nats"` instead.

### Postgres queue configuration (config.toml only)

When `queue_driver = "postgres"`, the following `[server]` section fields control the queue behaviour. **Note: these are TOML fields only, not environment variables.**

```toml
[server]
queue_driver = "postgres"

# Optional: Postgres DSN for the queue.
# If unset, defaults to the main DB_URL environment variable. task_queue
# lives in the same database as everything else (created by the same
# migration path), so this is normally the same DSN as the main store's,
# not a separate database.
postgres_queue_dsn = "postgres://user:pass@host:5432/db?sslmode=disable"

# How long (in seconds) a task lease is valid before expiry and requeue.
# Default: 300 (5 minutes)
postgres_queue_lease_ttl = 300

# Maximum number of retry attempts before a task moves to dead-letter.
# Default: 5
postgres_queue_max_attempts = 5
```

### Tests

```bash
# Unit tests only (no external dependencies)
make test-unit

# Integration tests (requires make infra first)
make test-integration

# Both
make test-all
```

See [docs/deployment.md](deployment.md) for running this in Docker, on bare metal, or under systemd.
