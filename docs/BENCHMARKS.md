# Benchmarks

Standard `go test -bench`, no extra tooling. Three areas are covered:

1. Cross-pod relay round-trip latency (SSE event fan-out and the answer/
   permission/cancel control plane) — requires a real NATS server.
2. The repo-index cache's real benefit — a cold `tfscan.Parse` versus a
   cache hit — no external services needed.
3. Guidance on extending this to a heavier load test against the full HTTP
   API, since this project doesn't have one and building a full harness was
   out of scope for the work that added these benchmarks.

## Running them

```bash
# Repo-index cache benefit (no external services needed):
go test -bench=. -run=^$ ./internal/tfscan/...
go test -bench=. -run=^$ ./internal/skills/...

# Cross-pod relay round-trip latency (needs NATS: `make infra`, or point
# NATS_URL at an existing server):
NATS_URL=nats://localhost:4222 go test -tags=integration -bench=. -run=^$ ./internal/server/...
```

`-run=^$` skips every non-benchmark `Test...` function so only the
`Benchmark...` functions execute — otherwise `go test -bench=.` still runs
the full test suite first. The NATS-gated benchmarks live in files tagged
`//go:build integration`, the same convention this repo already uses for
every other NATS-dependent test (`internal/queue/nats_test.go`,
`internal/server/event_relay_test.go`, `internal/server/control_relay_test.go`,
`internal/server/multireplica_e2e_test.go`) — `-tags=integration` is required
or the Go build simply won't see those files. Each NATS benchmark also skips
itself at runtime with `b.Skip` if `NATS_URL` isn't set, so
`go test -tags=integration ./...` without a NATS server still passes (just
skipping those benchmarks) rather than failing.

Add `-benchtime=200x` (or any `Nx`) to control how many iterations run —
useful to shorten a benchmark with a slow per-op cost (the control-relay one)
or get a more stable average on a fast one.

## What the numbers mean

Real numbers from one local run (Apple M4 Pro, real NATS in Docker on the
same machine — expect different absolute numbers on different hardware, but
the *relative* comparisons below should hold):

```
BenchmarkNATSEventRelay_PublishSubscribeRoundTrip-14       200    269253 ns/op   (~0.27ms)
BenchmarkNATSControlRelay_RequestRoundTrip-14              200    455314 ns/op   (~0.46ms)

BenchmarkRepoScan_ColdParse-14                             200    597162 ns/op   (~0.60ms)
BenchmarkRepoScan_CacheHit-14                              200    109228 ns/op   (~0.11ms)
```

- **`NATSEventRelay_PublishSubscribeRoundTrip`** — one connection publishes
  an SSE event, a second (standing in for a different pod's `ServeSSE`)
  receives it. This is the latency a client on a non-owning pod adds on top
  of whatever the owning pod's own processing takes, per event.
- **`NATSControlRelay_RequestRoundTrip`** — one connection sends an answer/
  permission/cancel request, a second (the "owning pod") satisfies it via its
  local handler and replies. Roughly double the event-relay latency, which
  makes sense: it's a NATS core request-reply (two network hops: request out,
  reply back) versus the event relay's one-way publish.
- **`RepoScan_ColdParse` vs `RepoScan_CacheHit`** — the repo-index cache
  (`internal/skills/repo_scan.go`) turns a ~0.6ms structural parse of a
  representative-sized repo (20 files, ~220 resource/module/variable/output/
  provider blocks — see `buildBenchRepoFiles` in
  `internal/skills/repo_scan_bench_test.go`) into a ~0.11ms cache read, about
  a 5x reduction on this fixture. The real-world win scales with repo size:
  a much larger repo makes the cold-parse side proportionally more expensive
  while the cache-hit side stays roughly flat (a JSON blob read + unmarshal).
  These two benchmarks deliberately measure `tfscan.Parse` and
  `store.GetRepoIndex` + `json.Unmarshal` directly rather than going through
  the full `terraformSummary(ctx, root)` — that function's first line always
  calls `gitIdentity(root)`, which shells out to `git` twice
  (`rev-parse HEAD`, `remote get-url origin`). That's a real cost paid on
  every call regardless of cache hit or miss, but it's a roughly constant
  ~1-2 subprocess-spawn cost that has nothing to do with the cache's own
  effect — leaving it in swamped the actual signal by 1-2 orders of magnitude
  during development of this benchmark (see the comment at the top of
  `internal/skills/repo_scan_bench_test.go` for how that surfaced). If you're
  trying to estimate real end-to-end `terraformSummary` latency rather than
  just the cache's isolated effect, add roughly 2 `git` subprocess spawns'
  worth of overhead (a few ms typically, highly OS/filesystem dependent) to
  both sides.
- **`internal/tfscan`'s own `BenchmarkParse`** (`go test -bench=. ./internal/tfscan/...`)
  is the same cold-parse cost in isolation, useful if you're changing
  `tfscan.Parse` itself and want a number that doesn't involve the cache or
  `internal/skills` at all.

None of these numbers are pass/fail gates — there's no benchmark assertion
in CI. They exist so a future change to the relay transport, the cache, or
the scanner has a real "before" number to diff against, instead of a guess.

## Extending this to a heavier load test

These benchmarks exercise individual mechanisms in isolation (one relay
round trip, one cache lookup). None of them drive the full HTTP API the way a
real deployment would — many concurrent clients submitting tasks, opening SSE
streams, and sending answer/permission/cancel requests against a running
server. That's a different, heavier kind of test, and building a full load
tool was out of scope for the work that added these benchmarks. Two
reasonable ways to get there later, roughly in order of effort:

1. **Point an existing HTTP load tool at the API.** `hey` or `wrk` can drive
   `POST /v1/tasks` at a fixed rate/concurrency and report latency
   percentiles and throughput directly:

   ```bash
   hey -n 200 -c 20 -m POST \
     -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"input":{"type":"prompt","text":"create an S3 bucket"},"output":{"type":"print"}}' \
     http://localhost:8080/v1/tasks
   ```

   This covers submission throughput well, but neither tool understands
   Server-Sent Events, so it won't exercise `GET /v1/tasks/{id}/stream` —
   see the next option for that.

2. **Write a small concurrent Go client.** A `main.go` (or a `go test`
   driver, run with `go run`, not `go test`, since this is a load-generation
   tool rather than a correctness test) that spawns N goroutines, each of
   which: submits a task, opens its SSE stream (a plain
   `http.Get`/`bufio.Scanner` reading `data: ...` lines is enough — no SSE
   library needed), optionally sends an answer/permission response if the
   task pauses, and records wall-clock time to the terminal event. Aggregate
   p50/p95/p99 submission-to-completion latency and throughput across all N
   goroutines. This is the only way to exercise the SSE path and the control
   endpoints (`/answer`, `/permission`, `/cancel`) under load, and it's the
   natural place to extend `internal/server/concurrency_test.go`'s in-process
   approach into an out-of-process one — same idea (many goroutines hammering
   owned/not-owned task IDs), but through the real HTTP server and, if you
   want to prove the multi-replica story specifically, against two or more
   real server processes behind a load balancer with `queue_driver=nats`
   rather than one process's in-memory `Hub`/`Runner`.

Either approach should be pointed at a server started with
`queue_driver=nats` (`make infra` for local NATS + Postgres) to exercise the
cross-pod relay paths this document's benchmarks measure in isolation — a
single-process `queue_driver=memory` run never touches
`NATSEventRelay`/`NATSControlRelay` at all.
