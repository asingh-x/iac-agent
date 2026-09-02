# Deploying to a Server

## Docker (recommended)

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

## Bare metal / VM

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

## systemd unit

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

See [docs/configuration.md](configuration.md) for the full config file / env var reference, and [docs/scaling.md](scaling.md) for what to change as you grow beyond a single instance.
