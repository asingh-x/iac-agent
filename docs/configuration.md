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

## Rotating the encryption key

`TF_AGENT_ENCRYPTION_KEY` accepts an optional `<keyID>:` prefix (e.g.
`v2:0102...`) — a bare 64-hex-char value with no prefix defaults to `v1`,
so existing deployments need no change. To rotate:

1. Move the current key into `TF_AGENT_ENCRYPTION_KEYS_OLD` (comma-separated
   `<keyID>:<hex>` pairs, decrypt-only) under its existing ID.
2. Set `TF_AGENT_ENCRYPTION_KEY` to a new value with a new `<keyID>:` prefix.
3. Restart. Already-stored tokens keep decrypting via the old-keys list;
   anything saved or updated after the restart is encrypted with the new
   current key.
4. Once you're confident every live token has been re-saved (e.g. after
   asking users to re-enter GitHub/Atlassian tokens via Settings), drop the
   old key from `TF_AGENT_ENCRYPTION_KEYS_OLD`.

There's no bulk re-encryption job — rotation is gradual, keyed by whichever
tokens users happen to re-save. If you need forced re-encryption of every
row on a timeline, that's a separate follow-up (not part of this feature).

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

See [docs/deployment.md](deployment.md) for running this in Docker, on bare metal, or under systemd.
