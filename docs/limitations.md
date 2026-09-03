# Known Limitations

| Area | Limitation |
|---|---|
| **Git provider** | GitHub only (`api.github.com`). GitLab, Bitbucket, and Gitea are not supported. |
| **PR base branch** | Hardcoded to `main`. Custom base branches are not yet configurable. |
| **Jira** | Atlassian Cloud only (REST API v3). On-premise Jira Server is not supported. |
| **IaC runtime** | `terraform` CLI only. OpenTofu and other Terraform forks are not supported. |
| **Security scanner** | `checkov` only. Tfsec, Terrascan, and other scanners are not integrated. |
| **LLM provider** | Claude models only (Anthropic API or AWS Bedrock). OpenAI, Gemini, and others are not supported. |
| **Toolchain** | `terraform`, `tflint`, and `checkov` run inside a sandboxed Docker container or Kubernetes Job (Docker is the default, configurable via `sandbox_backend` — see [docs/sandbox.md](sandbox.md)). The sandbox image is published to `ghcr.io/asingh-x/iac-agent/sandbox:latest` and works out of the box; optionally, you can build your own image locally with `make sandbox-build`. Direct installation on the host is no longer required or supported. |
| **TLS** | No built-in TLS. Requires a terminating reverse proxy (nginx, Caddy, ALB) in production. |

These are documented gaps, not oversights — see [docs/roadmap.md](roadmap.md) for what's planned to close them.
