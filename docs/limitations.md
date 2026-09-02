# Known Limitations

| Area | Limitation |
|---|---|
| **Git provider** | GitHub only (`api.github.com`). GitLab, Bitbucket, and Gitea are not supported. |
| **PR base branch** | Hardcoded to `main`. Custom base branches are not yet configurable. |
| **Jira** | Atlassian Cloud only (REST API v3). On-premise Jira Server is not supported. |
| **IaC runtime** | `terraform` CLI only. OpenTofu and other Terraform forks are not supported. |
| **Security scanner** | `checkov` only. Tfsec, Terrascan, and other scanners are not integrated. |
| **LLM provider** | Claude models only (Anthropic API or AWS Bedrock). OpenAI, Gemini, and others are not supported. |
| **Toolchain** | `terraform`, `tflint`, and `checkov` must be installed on the host, unless the optional sandbox is enabled (`sandbox_enabled = true` — Docker or Kubernetes backend, see [docs/sandbox.md](sandbox.md)). Direct host execution is still the default. |
| **TLS** | No built-in TLS. Requires a terminating reverse proxy (nginx, Caddy, ALB) in production. |

These are documented gaps, not oversights — see [docs/roadmap.md](roadmap.md) for what's planned to close them.
