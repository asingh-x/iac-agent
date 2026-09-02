# iac-agent

![License](https://img.shields.io/badge/license-MIT-blue)
![Go](https://img.shields.io/badge/go-1.26-00ADD8)

> Near-deterministic IaC automation — takes a Terraform task and delivers a validated GitHub PR, fully autonomous, end-to-end, in under 10 minutes.

The server is the core of the system. The client can be a web app, a CLI, or a binary like a VS Code plugin — we chose a web client because it's easier to distribute and manage.

---

## Demo

A real run against a live repo — prompt in, agent scans the repo, writes HCL, runs `terraform validate` and `checkov`, fixes what it can, and opens the PR. Real time during the interactive parts, fast-forwarded through the agent's actual thinking/streaming stretch.

![iac-agent demo](docs/assets/demo.gif)

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

## Quick start

**Requirements:** Go 1.26+, Node 20+, an Anthropic API key (or AWS Bedrock credentials), and a running Docker daemon. `terraform` / `tflint` / `checkov` always run inside the [sandbox](docs/sandbox.md) container — they're never invoked on the host, so you don't need them on `PATH`.

```bash
git clone https://github.com/asingh-x/iac-agent
cd iac-agent

export ANTHROPIC_API_KEY=sk-...
make run
```

Open [http://localhost:8080](http://localhost:8080) and log in with the admin token printed on first start.

---

## Documentation

| Doc | What's in it |
|---|---|
| [docs/feat.md](docs/feat.md) | Full capability reference, by category |
| [docs/architecture.md](docs/architecture.md) | System diagram, request flow, data model, security boundaries, enterprise readiness |
| [docs/configuration.md](docs/configuration.md) | Config file, env vars, infra bootstrap, secrets handling |
| [docs/deployment.md](docs/deployment.md) | Docker, bare metal, and systemd deployment |
| [docs/scaling.md](docs/scaling.md) | Scaling by team size, what to swap out first as you grow |
| [docs/api.md](docs/api.md) | Full REST API reference |
| [docs/sandbox.md](docs/sandbox.md) | Sandboxed `terraform`/`tflint`/`checkov` execution (Docker or Kubernetes) |
| [docs/benchmarks.md](docs/benchmarks.md) | Real performance numbers and how to reproduce them |
| [docs/limitations.md](docs/limitations.md) | Known limitations |
| [docs/roadmap.md](docs/roadmap.md) | What's still open |
| [docs/aws-production.md](docs/aws-production.md) | Phase-2 AWS production migration |
| [CHANGELOG.md](CHANGELOG.md) | Release history |

---

## Get in touch

If you're building agentic systems for DevOps or IaC, or just want to explore — [connect on LinkedIn](https://www.linkedin.com/in/ciaoavinash/).

## Contributing

Contributions are welcome. Please open a PR — see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines, [docs/roadmap.md](docs/roadmap.md) for planned work, and [SECURITY.md](SECURITY.md) for reporting vulnerabilities.

## License

MIT
