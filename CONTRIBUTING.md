# Contributing

## Prerequisites

- Go 1.25+
- Node 20+
- `terraform` CLI (for validate skill tests)
- `checkov` (for security scan skill tests)

## Development setup

```bash
git clone https://github.com/asingh-x/iac-agent
cd iac-agent

# Install client dependencies
cd client && npm install && cd ..

# Run tests
go test ./...
```

## Running locally

```bash
export ANTHROPIC_API_KEY=sk-...
make run
```

The server watches nothing — rebuild and restart manually after Go changes:
```bash
make build && lsof -ti :8080 | xargs kill -9 && .bin/tf-agent-server &
```

For client changes Vite HMR is available:
```bash
cd client && npm run dev   # proxies API to :8080
```

## Project conventions

**Go**
- Standard library `net/http` only — no external router
- Register admin routes directly on the root mux with `method /path/{id}` patterns; never in a sub-mux
- Keep `internal/tools` free of imports from `internal/agent` — use injected function types to break cycles
- Add new store methods to the `Store` interface in `store.go` first, then implement in `postgres.go`
- When adding DB columns, write an `ALTER TABLE ADD COLUMN` migration — do not drop and recreate tables

**TypeScript**
- Types live in `client/src/types.ts`
- API calls live in `client/src/lib/api.ts`
- Components are function components; no class components

**Commits**
- One logical change per commit
- Prefix: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`

## Adding a skill

1. Create `internal/skills/my_skill.go` implementing the `skills.Skill` interface
2. Add a prompt template in `internal/skills/prompts/my_skill.md` if needed
3. Register in `internal/server/task_runner.go` → `wireAgent()`
4. Add tests in `internal/skills/skills_test.go`

## Adding a tool

1. Create `internal/tools/my_tool.go` implementing the `Tool` interface
2. Register in `wireAgent()` and, if role-scoped, in `buildSubAgentRunner()`
3. Mark `IsDestructive` and `IsReadOnly` accurately — the permission checker uses these

## Pull requests

- Keep PRs focused; one feature or fix per PR
- Include tests for new behaviour
- Update `CHANGELOG.md` under `[Unreleased]`
