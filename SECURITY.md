# Security

## Reporting a vulnerability

Please do **not** open a public GitHub issue for security vulnerabilities.

Instead, [create a GitHub Security Advisory](https://github.com/asingh-x/tf-agent/security/advisories/new) with:
- A description of the vulnerability
- Steps to reproduce
- Potential impact
- Any suggested mitigations

You will receive an acknowledgement within 48 hours and a status update within 7 days.

## Security model

### Authentication

All API endpoints require a bearer token (`Authorization: Bearer tfa-...`).

- Tokens are 160-bit random values (`crypto/rand`) prefixed with `tfa-`
- Only the SHA-256 hash of the token is stored in the database — the raw token is shown once and never retrievable
- Tokens can be revoked (hash cleared, account deactivated) or rotated by an admin at any time

### Authorisation

Two roles: `admin` and `member`.

- Members can only access their own tasks and settings
- Admins can manage all users via `/v1/admin/*` endpoints
- Self-action guards prevent an admin from deleting or deactivating their own account

### Stored secrets

Per-user GitHub and Atlassian tokens are encrypted at rest using AES-256-GCM before being written to the database. The encryption key is derived from `ENCRYPTION_KEY` in the server environment.

### Tool permissions

The agent runs with a configurable permissions policy (`config.toml → permissions.default`):

| Policy | Behaviour |
|---|---|
| `auto` | All non-destructive tool calls proceed automatically |
| `confirm` | Destructive tool calls require explicit approval |
| `deny` | Destructive tool calls are blocked |

Tools declare `IsDestructive()` and `IsReadOnly()` — the permission checker enforces policy based on these.

### Sub-agents

Sub-agents spawned by the `Agent` tool receive a restricted tool subset based on their role. The `security-auditor` and `reviewer` roles are read-only. The `coder` and `tester` roles have write and Bash access but cannot call external APIs.

### Network

- The server listens on `localhost:8080` by default — do not expose directly to the internet without a TLS-terminating reverse proxy
- CORS is not enabled; the bundled SPA is served from the same origin

## Known limitations

- The PostgreSQL database is not encrypted at rest beyond the column-level AES-256-GCM for token fields — protect the database with filesystem permissions and network access controls
- Bash tool execution is sandboxed only by OS-level user permissions; run the server as a least-privilege user
