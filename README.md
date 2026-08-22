# iac-agent

![License](https://img.shields.io/badge/license-MIT-blue)
![Go](https://img.shields.io/badge/go-1.26-00ADD8)

> Near-deterministic IaC automation — takes a Terraform task and delivers a validated GitHub PR, fully autonomous, end-to-end, in under 10 minutes.

The server is the core of the system. The client can be a web app, a CLI, or a binary like a VS Code plugin — we chose a web client because it's easier to distribute and manage.

---

## Table of contents

- [Why iac-agent?](#why-iac-agent)
- [Features](#features)
- [Architecture](#architecture)
- [How it stays fast](#how-it-stays-fast)
- [Focused Skills](#focused-skills)
- [Infrastructure setup](#infrastructure-setup)
- [Scaling by team size](#scaling-by-team-size)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Deploying to a server](#deploying-to-a-server)
- [API](#api)
- [Known limitations](#known-limitations)
- [Roadmap](#roadmap)
- [Get in touch](#get-in-touch)
- [Contributing](#contributing)
- [License](#license)

---

## Why iac-agent?

Every time you ask ChatGPT or Claude to write Terraform, **you become the bottleneck** — copy the output, run the linter, fix the error, paste it back, repeat. iac-agent removes you from the execution loop entirely.

| | ChatGPT / Claude | iac-agent |
|---|---|---|
| Writes Terraform HCL | ✓ | ✓ |
| Runs `terraform validate` | ✗ — you do it | ✓ automatic |
| Runs `checkov` security scan | ✗ — you do it | ✓ automatic |
| Reads your existing repo conventions | ✗ | ✓ via RepoScan |
| Asks clarifying questions before coding | ✗ | ✓ pauses and waits |
| Opens a GitHub PR | ✗ — you do it | ✓ automatic |
| Pulls context from a Jira ticket | ✗ | ✓ native input type |
| Detects infrastructure drift | ✗ | ✓ via DriftDetect |
| Streams live progress | ✗ | ✓ SSE in real time |
| Time from prompt to merged PR | 30–60 min (human in loop) | **< 10 min** |

**Human in at the start. Human in at the review. Autonomous everything in between.**

The next generation of AI tooling isn't smarter chat. It's specialized skills, async execution, persistent state, and humans at the review gate — not the keyboard.

---

## Features

| Feature | Detail |
|---|---|
| **End-to-end pipeline** | Repo scan → clarify → generate → validate → security scan → PR, fully chained |
| **Jira integration** | Submit a ticket ID as input; agent reads the ticket and implements it |
| **Live streaming** | Every step streamed to the UI via SSE — no black box |
| **Mid-task questions** | Agent pauses, asks a clarifying question, waits up to 7 days for your answer (configurable), continues |
| **Sub-agents** | Spawns isolated reviewer / coder / tester / security-auditor sub-agents for focused tasks |
| **Drift detection** | Compares live infrastructure state against Terraform code |
| **Multi-user** | Admin/member roles, per-user API keys (`tfa-` format), token revocation |
| **Encrypted secrets** | GitHub and Atlassian tokens stored AES-256-GCM encrypted at rest |
| **Multi-provider LLM** | Anthropic API or AWS Bedrock — swap in config |
| **Configurable permissions** | `auto` / `confirm` / `deny` policy for destructive tool calls |
| **Prometheus metrics** | Task duration, token usage, throughput at `/metrics` |
| **Multi-replica safe** | Run N pods behind a plain load balancer — no sticky sessions. SSE streaming and answer/permission/cancel requests work no matter which pod a request lands on (see [docs/architecture.md](docs/architecture.md)) |
| **Sandboxed execution** | Optional Docker or Kubernetes backend runs `terraform`/`tflint`/`checkov` network-isolated, non-root, resource-limited — for untrusted multi-tenant use (see [docs/sandbox.md](docs/sandbox.md)) |
| **Audit trail** | Every admin action (user create/update/delete/activate/token regen) is logged with actor, action, and target — queryable at `/v1/admin/audit-log` |

---

## Architecture

The system diagram, request flow, data model, security boundaries, and enterprise readiness (RBAC, audit trail, sandboxed execution, HA) live in [docs/architecture.md](docs/architecture.md).

---

## How it stays fast

| Technique | Impact |
|---|---|
| Smart repo comprehension | Extracts only structurally relevant context — not the entire codebase |
| Skill specialisation | Each skill has a tightly scoped prompt optimised for one job |
| Prompt caching | Avoids re-sending large repeated context on every LLM call |
| Minimal LLM turns | Every unnecessary round-trip adds seconds; 8 skills compounds fast |
| Async validation | `tflint` and `terraform validate` run in parallel — not sequentially |

---

## Focused Skills

| Skill | Description |
|---|---|
| `RepoScan` | Scans reference repos for naming conventions and module patterns |
| `Clarifier` | Asks targeted follow-up questions before writing any code |
| `Generate` | Writes Terraform HCL matched to your repo's conventions |
| `Validate` | Runs `terraform validate` and fixes errors automatically |
| `SecurityScan` | Runs `checkov` static analysis, surfaces policy violations |
| `CreatePR` | Opens a GitHub pull request with the generated code |
| `JiraFetch` | Reads a Jira ticket and uses it as the task specification |
| `DriftDetect` | Detects drift between Terraform code and live infrastructure |

---

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
| `QUEUE_DRIVER` | `memory` | `memory` or `nats` |
| `NATS_URL` | `nats://127.0.0.1:4222` | NATS server URL |
| `QUEUE_NAMES` | `default` | Comma-separated named queues — each gets its own worker goroutine (e.g. `default,security`) |
| `TF_AGENT_ADMIN_TOKEN` | — | Bootstrap admin token on first run |

### Tests

```bash
# Unit tests only (no external dependencies)
make test-unit

# Integration tests (requires make infra first)
make test-integration

# Both
make test-all
```

---

## Scaling by team size

| Component | Small (≤ 10) | Mid-size (10–50) | Large (50–100+) |
|---|---|---|---|
| **Database** | PostgreSQL | PostgreSQL | PostgreSQL — connection pooling, read replicas |
| **Queue** | In-memory (default) | **NATS JetStream** (required for multiple replicas) | **NATS JetStream** — multiple agent workers, per-queue routing |
| **LLM provider** | Anthropic API | Anthropic API | **AWS Bedrock** — no rate limits, private VPC, SOC2 |
| **Agent workers** | 1 process | 1–3 processes | Horizontal pod autoscaling (Kubernetes) |
| **Storage** | Local filesystem | Local or EFS-backed volume | **EFS / S3** via Kubernetes PVC — state survives restarts, cloud-agnostic (AWS/GCP/Azure) |
| **Auth** | Built-in token auth | Built-in token auth | SSO via reverse proxy (Okta, Entra ID) |
| **Secrets** | Env vars | Env vars | **AWS Secrets Manager / Vault** |
| **Observability** | Logs + `/metrics` | Prometheus + Grafana | Prometheus + Grafana + distributed tracing |

### What to swap out first as you grow

**In-memory queue → NATS JetStream** — **required** the moment you run more than one replica, not just a nice-to-have. With the default in-memory queue every piece of per-task state is process-local: the queue itself, SSE streams, and the answer / permission / cancel endpoints. A second replica means a client's stream can land on a pod that isn't running its task, and an approval can be POSTed to a pod that never sees it. Switching to NATS also switches those paths onto a cross-pod relay, automatically — there's no separate flag. It's also what gets you per-team queues (e.g. `default,security`) and durable redelivery if a worker crashes.

```toml
[server]
queue_driver = "nats"
nats_url     = "nats://nats:4222"
# QUEUE_NAMES env var controls which named queues this instance processes
```

**Anthropic API → AWS Bedrock** — when you need private network access, no external rate limits, or enterprise compliance (HIPAA, SOC2).

```toml
[provider]
name = "bedrock"

[provider.bedrock]
region = "us-east-1"
model  = "us.anthropic.claude-opus-4-6-20251101-v1:0"
```

---

## Requirements

- Go 1.26+
- Node 20+ (for the web UI)
- An Anthropic API key (or AWS Bedrock credentials)
- `terraform` CLI, `tflint`, `checkov` (for validate/security skills)

## Quick start

```bash
git clone https://github.com/asingh-x/iac-agent
cd iac-agent

# Set your API key
export ANTHROPIC_API_KEY=sk-...

# Build and run
make run
```

Open [http://localhost:8080](http://localhost:8080) and log in with the admin token printed on first start.

## Configuration

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

### Env vars vs config file

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

---

## Deploying to a server

### Docker (recommended)

```bash
docker build -t tf-agent .

docker run -d \
  --name tf-agent \
  -p 8080:8080 \
  -e ANTHROPIC_API_KEY="sk-ant-..." \
  -e DB_DRIVER=postgres \
  -e DB_URL="postgres://tfagent:pass@db:5432/tfagent?sslmode=disable" \
  -e QUEUE_DRIVER=nats \
  -e NATS_URL="nats://nats:4222" \
  -e QUEUE_NAMES="default,security" \
  -e TF_AGENT_ADMIN_TOKEN="tfa-..." \
  tf-agent
```

On first start the admin user is created. The raw token is printed once — save it.

### Bare metal / VM

```bash
# 1. Build
make build

# 2. Write config (non-secret settings only)
mkdir -p ~/.tf-agent
cat > ~/.tf-agent/config.toml <<EOF
[server]
port = 8080
db_driver  = "postgres"
queue_driver = "nats"
nats_url   = "nats://127.0.0.1:4222"

[provider]
name  = "anthropic"
model = "claude-opus-4-6"
EOF

# 3. Run (pass secrets as env vars)
ANTHROPIC_API_KEY="sk-ant-..." \
DB_URL="postgres://tfagent:pass@127.0.0.1:5432/tfagent?sslmode=disable" \
QUEUE_NAMES="default,security" \
TF_AGENT_ADMIN_TOKEN="tfa-$(openssl rand -hex 20)" \
.bin/tf-agent-server
```

### systemd unit

```ini
[Unit]
Description=tf-agent server
After=network.target postgresql.service

[Service]
User=tf-agent
EnvironmentFile=/etc/tf-agent/secrets.env
ExecStart=/opt/tf-agent/tf-agent-server
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

`/etc/tf-agent/secrets.env` (mode `0600`, owned by root):

```
ANTHROPIC_API_KEY=sk-ant-...
DB_URL=postgres://tfagent:pass@localhost:5432/tfagent?sslmode=disable
TF_AGENT_ADMIN_TOKEN=tfa-...
```

## API

All endpoints require `Authorization: Bearer <tfa-token>`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/me` | Current user info |
| `POST` | `/v1/tasks` | Submit a task |
| `GET` | `/v1/tasks/{id}` | Get task details |
| `GET` | `/v1/tasks/{id}/stream` | SSE stream for live output |
| `POST` | `/v1/tasks/{id}/answer` | Send answer to a waiting task |
| `POST` | `/v1/tasks/{id}/cancel` | Cancel a running task |
| `GET` | `/v1/tasks` | List recent tasks |
| `GET` | `/v1/settings` | Get user settings |
| `PUT` | `/v1/settings` | Update user settings |

Admin-only:

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/admin/users` | List users |
| `POST` | `/v1/admin/users` | Create user |
| `PATCH` | `/v1/admin/users/{id}` | Update username / role |
| `DELETE` | `/v1/admin/users/{id}` | Delete user |
| `POST` | `/v1/admin/users/{id}/activate` | Activate user |
| `POST` | `/v1/admin/users/{id}/deactivate` | Deactivate user |
| `POST` | `/v1/admin/users/{id}/token` | Regenerate API key |
| `DELETE` | `/v1/admin/users/{id}/token` | Revoke API key |

## Roadmap

See [docs/roadmap.md](docs/roadmap.md) for what's still open, and [CHANGELOG.md](CHANGELOG.md) for what's already shipped.

---

## Known limitations

| Area | Limitation |
|---|---|
| **Git provider** | GitHub only (`api.github.com`). GitLab, Bitbucket, and Gitea are not supported. |
| **PR base branch** | Hardcoded to `main`. Custom base branches are not yet configurable. |
| **Jira** | Atlassian Cloud only (REST API v3). On-premise Jira Server is not supported. |
| **IaC runtime** | `terraform` CLI only. OpenTofu and other Terraform forks are not supported. |
| **Security scanner** | `checkov` only. Tfsec, Terrascan, and other scanners are not integrated. |
| **LLM provider** | Claude models only (Anthropic API or AWS Bedrock). OpenAI, Gemini, and others are not supported. |
| **Toolchain** | `terraform`, `tflint`, and `checkov` must be installed on the host, unless the optional sandbox is enabled (`sandbox_enabled = true` — Docker or Kubernetes backend, see [docs/sandbox.md](docs/sandbox.md)). Direct host execution is still the default. |
| **TLS** | No built-in TLS. Requires a terminating reverse proxy (nginx, Caddy, ALB) in production. |

---

## Get in touch

If you're building agentic systems for DevOps or IaC, or just want to explore — [connect on LinkedIn](https://www.linkedin.com/in/ciaoavinash/).

---

## Contributing

Contributions are welcome. Please open a PR — see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

See [CHANGELOG.md](CHANGELOG.md) for release history, [docs/roadmap.md](docs/roadmap.md) for planned work, and [SECURITY.md](SECURITY.md) for reporting vulnerabilities.

## License

MIT
