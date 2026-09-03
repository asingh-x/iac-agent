# Scaling by Team Size

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

## What to swap out first as you grow

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

See [docs/aws-production.md](aws-production.md) for the full AWS production migration path once you're operating at the "Large" tier above.
