# Terraform execution sandbox

## Why

`internal/skills/validate.go` and `internal/skills/security_scan.go` shell
`terraform`, `tflint`, and `checkov` out to the **host** process, in the
task's working directory. That gives arbitrary LLM-generated HCL and
arbitrary task input the same privileges and network access as the tf-agent
server process itself — acceptable for a single trusted operator, a real gap
once multiple untrusted users can submit tasks. Everything below is already
built; making it mandatory rather than opt-in is tracked in
[docs/roadmap.md](roadmap.md) under On-Premises Rollout as "Mandatory
sandboxing + real auth".

## What's built (Docker, local/single-node)

- **`internal/sandbox`** — a small `Executor` interface:

  ```go
  type Executor interface {
      Run(ctx context.Context, workDir string, name string, args ...string) (output string, err error)
  }
  ```

  `name`/`args` mean exactly what they mean to `os/exec` — this is a
  drop-in replacement for the `runCmd(ctx, dir, name, args...)` helper
  `ValidateSkill` always used, not a new calling convention.

- **`sandbox.DockerExecutor`** — the only implementation today. Runs the
  command inside a container via the `docker` CLI (`exec.CommandContext`
  invoking `docker run ...`, the same shell-out pattern
  `internal/tools/bash.go` already uses — no Docker SDK dependency). Every
  run gets:
  - `--rm` plus a generated `--name` and an unconditional `docker rm -f`
    backstop afterward, so a hard-killed run (context cancellation/timeout)
    can never leak a container — `--rm` alone only fires on a clean `docker`
    CLI exit, not on a `SIGKILL` to the client process.
  - `-v <workDir>:/workspace:rw -w /workspace` — the working directory is
    bind-mounted, matching the role `cmd.Dir` played for the host-exec path.
  - `--network=none` — none of `terraform validate` / `tflint` / `checkov`
    need network access for local validation. Verified for real (see
    below), not assumed.
  - `--memory` / `--cpus` (configurable, default `512m` / `1`).
  - `--cap-drop=ALL --security-opt=no-new-privileges`, and no `--user`
    override — the container runs as the non-root user baked into the
    image.

- **`docker/sandbox/Dockerfile`** — a separate image from the top-level
  `Dockerfile` (which builds the tf-agent server itself). Two stages: a
  `debian:bookworm-slim` fetcher stage downloads pinned `terraform` and
  `tflint` release binaries, and a `python:3.12-slim` final stage installs a
  pinned `checkov` via pip and creates a non-root user (`sandbox`, uid
  10001). No `ENTRYPOINT`/`CMD` — `DockerExecutor` supplies the full command.
  Build with `make sandbox-build`.

  If you're building behind a TLS-inspecting corporate proxy and the build
  fails with a certificate error, drop your proxy's root CA as a `.crt` file
  into `docker/sandbox/certs/` (gitignored) — the Dockerfile's
  `update-ca-certificates` step picks it up. This only ever *adds* trust for
  a cert you explicitly supply; it never disables verification.

- **Config** (`internal/config/settings.go`, `ServerConfig`):
  `sandbox_enabled` (bool, **default `false`**), `sandbox_image` (default
  `iac-agent-sandbox:latest`), `sandbox_memory` (default `512m`),
  `sandbox_cpus` (default `1`). Env overrides: `TF_AGENT_SANDBOX_ENABLED`,
  `TF_AGENT_SANDBOX_IMAGE`. See `config.sample.toml`.

  `sandbox_enabled` defaults to `false` so `make run`/local dev keeps
  working unchanged for anyone without Docker running — the sandbox is
  opt-in, not a surprise new dependency.

- **Wiring** (`internal/server/task_runner.go: wireAgent`): when
  `sandbox_enabled = true`, a `sandbox.DockerExecutor` is built from
  config and passed to `skills.NewValidateSkill(executor)` and
  `skills.NewSecurityScanSkill(executor)`. When `false` (default),
  `executor` is `nil` and both skills fall back to their original
  direct-host `exec.CommandContext` path — byte-for-byte the same code path
  that ran before `internal/sandbox` existed.

- **`make doctor`** prints whether the sandbox image has been built
  (`docker image inspect <sandbox_image>`) and points at
  `make sandbox-build` if not.

## Enabling it

```bash
make sandbox-build                 # builds iac-agent-sandbox:latest
```

```toml
# ~/.tf-agent/config.toml
[server]
sandbox_enabled = true
# sandbox_image  = "iac-agent-sandbox:latest"  # default
# sandbox_memory = "512m"                     # default
# sandbox_cpus   = "1"                        # default
```

or `TF_AGENT_SANDBOX_ENABLED=true` in the environment.

## Verified for real (2026-08-22)

Built the image, ran real `terraform init`/`validate`, `tflint`, and
`checkov` invocations through `DockerExecutor` against fixture directories,
and confirmed:

- Real tool output comes back (`terraform`: "Success! The configuration is
  valid."; `tflint`: real JSON `{"issues":[...],"errors":[]}`; `checkov`: a
  real pass/fail count against a deliberately-misconfigured `aws_s3_bucket`
  resource).
- `--network=none` is a real containment property, not isolation theater: a
  direct socket-connect attempt from inside the container
  (`socket.create_connection(('8.8.8.8', 53))`) fails with `OSError: [Errno
  101] Network is unreachable`; the identical probe without `--network=none`
  succeeds, confirming the probe itself is valid and the flag is what's
  blocking it. Independently, checkov's own attempt to phone home to its
  guidelines API failed with a DNS resolution error under `--network=none`
  while it still produced complete, correct local scan output — checkov
  degrades gracefully with network access blocked.
- Cancellation cleans up: killing the `docker run` client process hard
  (simulating `ctx` cancellation) while a container is running leaves the
  container running in the daemon — `docker ps` still shows it — exactly the
  leak the `--rm`-alone approach is vulnerable to. The `docker rm -f`
  backstop then removes it, and `docker ps` shows nothing afterward. The
  automated version of this (`internal/sandbox` `TestDockerExecutor_
  CancelKillsContainerNoLeak`) exercises the same thing through the real
  `DockerExecutor.Run` code path: a 2s context timeout against a 60s sleep
  returns promptly and leaves no container behind.
- The non-root user is enforced: `docker run --rm iac-agent-sandbox:latest id`
  reports `uid=10001(sandbox)`, not root.

See `internal/sandbox/docker_test.go` for the automated versions of these
checks (skip cleanly via `exec.LookPath("docker")` and a
`docker image inspect` check when Docker or the sandbox image aren't
available, matching this repo's existing `NATS_URL`-gated test pattern).

## Extension point

`sandbox.Executor` is the seam. Anything that can run
`name(args...)` against a working directory and return stdout and stderr
separately can implement it — `ValidateSkill`/`SecurityScanSkill` never need
to change again to add a new backend. Stdout/stderr are kept separate rather
than merged specifically because `SecurityScanSkill` JSON-parses checkov's
stdout, and checkov reliably writes a non-JSON warning to stderr under
`--network=none` (it can't reach its guidelines-mapping API) — merging the
two broke that parse. `ValidateSkill` doesn't parse its output as structured
data, so it just concatenates both for display.

## Kubernetes Job executor (`K8sJobExecutor`)

**Verified for real (2026-08-22)** against a live 4-node `kind` cluster
(`kind-test-dev`: 1 control-plane + 3 workers, all `amd64`, `kindnetd`
CNI, `local-path` — `rancher.io/local-path` — as the only, RWO-only
storage class), in a brand-new `iac-agent-sandbox` namespace created for
this work (never touched any pre-existing namespace on that shared
cluster).

### What's built

- **`internal/sandbox/kubernetes.go`** — `K8sJobExecutor`, implementing the
  same `Executor` interface as `DockerExecutor`. For each `Run`: creates a
  Kubernetes `Job` (one pod, `backoffLimit: 0`), waits for the pod to reach
  `Running`, stages `workDir` in, execs the real command, then always
  deletes the Job (and force-deletes any leftover pod as a backstop) on the
  way out — success, command failure, or `ctx` cancellation all take the
  same cleanup path, via a `defer` on a fresh short-lived context so cleanup
  still runs even if `Run`'s own `ctx` is already cancelled/expired. This is
  the direct Kubernetes analogue of `DockerExecutor`'s `--rm` +
  `docker rm -f` backstop.
- **Security posture**, mirroring `DockerExecutor` wherever Kubernetes has a
  direct equivalent: `RunAsNonRoot: true` + `RunAsUser: 10001` (matching the
  `sandbox` user baked into `docker/sandbox/Dockerfile` — the same image,
  just scheduled differently), `securityContext.capabilities.drop: ["ALL"]`,
  `allowPrivilegeEscalation: false`, `automountServiceAccountToken: false`,
  and per-container `resources.requests`/`limits` (`SandboxKubeMemory`/
  `SandboxKubeCPUs`, default `512Mi`/`1`).
- **Config** (`internal/config/settings.go`, `ServerConfig`):
  `sandbox_backend` (`"docker"` default | `"kubernetes"`),
  `sandbox_kube_namespace` (default `"iac-agent-sandbox"` — must already
  exist; `K8sJobExecutor` never creates, modifies, or deletes namespaces),
  `sandbox_kubeconfig_path` (empty uses client-go's standard resolution:
  `$KUBECONFIG`, then `~/.kube/config`, then in-cluster config when running
  inside a pod), `sandbox_kube_memory`/`sandbox_kube_cpus` (Kubernetes
  `resource.Quantity` syntax, e.g. `"512Mi"` — **not** the same syntax as
  `sandbox_memory`/`sandbox_cpus`, which are Docker `--memory`/`--cpus`
  values; Kubernetes parses a bare `m` suffix as milli, not megabytes, so
  the two aren't interchangeable strings). Env overrides:
  `TF_AGENT_SANDBOX_BACKEND`, `TF_AGENT_SANDBOX_KUBE_NAMESPACE`,
  `TF_AGENT_SANDBOX_KUBECONFIG_PATH`.
- **Wiring** (`internal/server/task_runner.go: wireAgent`): when
  `sandbox_enabled = true` and `sandbox_backend = "kubernetes"`, a
  `sandbox.K8sJobExecutor` is built instead of `DockerExecutor` and passed
  to the same `ValidateSkill`/`SecurityScanSkill` constructors — neither
  skill knows or cares which backend it's talking to. A failure to build
  the Kubernetes client (bad/unreachable kubeconfig) fails task setup
  immediately and loudly, rather than silently falling back to host-exec.

### File staging: exec/tar, not PVC/object-storage

The design sketch this section used to carry proposed two heavier options —
an object-storage round trip, or a per-task PVC — because of "the
volume-mounting problem": a Job's pod can land on any node, so there's no
one-line bind mount like Docker's `-v <workDir>:/workspace`. Before building
either, this was prototyped against the real cluster: the Job's pod runs a
long-enough-lived placeholder command (`sleep 3600` — a safety-net ceiling
only; `Run`'s own explicit delete is the real cleanup path, the same
"backstop's backstop" relationship `DockerExecutor` has between `--rm` and
`docker rm -f`), and once it's `Running`, `Run` execs `tar -xf - -C
/workspace` into it with stdin wired to a tar stream built from `workDir` on
this machine — the same mechanism `kubectl cp` uses under the hood, via
`k8s.io/client-go/tools/remotecommand`. The real command then runs the same
way, via a second exec call.

**This worked cleanly and is what shipped** — confirmed with real multi-file,
nested-subdirectory `workDir` fixtures actually landing intact inside the
pod (`internal/sandbox/kubernetes_test.go:
TestK8sJobExecutor_RealFileStaging`). It needs zero extra cluster
infrastructure: no PVC, no storage class requirements (this cluster's only
storage class, `local-path`, is RWO-only and would have been a real
constraint for the PVC approach — a `tf-agent` pod and a Job pod landing on
different nodes couldn't share a `ReadWriteOnce` volume), no object storage
round trip, no extra latency beyond the tar transfer itself (sub-second for
realistic `workDir` sizes — a handful of `.tf` files).

The real command also runs via exec (not as the Job's own container
command), so its stdout/stderr are captured directly and separately from
the exec stream — matching the `Executor` contract exactly — rather than
needing to parse the pod's combined log output, which was the original
sketch's Step 4. This turned out to be simpler than the original design,
not just cheaper.

### NetworkPolicy: verified enforced on this cluster (a real, non-obvious finding)

`K8sJobExecutor` creates (idempotently, per namespace) a deny-all-egress
`NetworkPolicy` — an empty `podSelector` (every pod in the namespace),
`policyTypes: [Egress]`, no `egress` rules — the closest cluster-level
equivalent to `DockerExecutor`'s `--network=none`.

Older kind CNI builds (`kindnetd`) were widely known **not** to enforce
`NetworkPolicy` at all, so this was checked for real rather than assumed —
and the result is the opposite of that old conventional wisdom. A controlled
A/B probe (raw-IP `wget` from inside a sandboxed pod, no DNS involved, the
same rigor as this doc's own Docker `--network=none` verification):

- **With** the deny-egress `NetworkPolicy` in place: `wget` to a raw
  external IP timed out — no egress.
- **With the identical policy deleted** (control, to confirm the probe
  itself was valid and not just a dead route): the same `wget` call
  succeeded and returned real HTTP content.

This cluster's `kindnetd` build (`docker.io/kindest/kindnetd:v20260528-...`
— a recent 2026 release) **does enforce `NetworkPolicy`**, at least for
this deny-all-egress case. This is cluster/CNI-version-specific, not a
property of Kubernetes generally — confirm it for your own cluster before
relying on it; don't assume either direction. (This probe is not part of
the committed test suite — the correct assertion direction is
cluster-specific, so asserting either outcome as "correct" would be a false
failure against a different CNI. It's a one-off verification, recorded
here.)

### What's verified vs. what's still open

Verified for real against the live cluster
(`internal/sandbox/kubernetes_test.go`, gated behind `requireK8s` — skips
cleanly via `kubectl` binary presence + a live namespace-reachability check
when no cluster is available, matching this repo's existing Docker/NATS
integration-test pattern):

- Real Job/pod creation, the pod actually running as uid `10001` (not
  root) — `TestK8sJobExecutor_RealCommandAndUser`.
- Real multi-file, nested-directory file staging via exec/tar —
  `TestK8sJobExecutor_RealFileStaging`.
- Cancellation genuinely cleans up: a 3-second `ctx` timeout against a
  300-second sleep returns promptly with no Job/Pod left behind
  (`kubectl get jobs,pods -n <namespace>` empty afterward) —
  `TestK8sJobExecutor_CancelCleansUpNoLeak`.
- The deny-egress `NetworkPolicy` object's shape is correct —
  `TestK8sJobExecutor_NetworkPolicyStructure`.
- Manually (not committed, since the correct outcome is cluster-specific):
  the `NetworkPolicy` enforcement A/B test above, and a real
  `terraform init`/`validate` (`Success! The configuration is valid.`)
  through `K8sJobExecutor` using the public `hashicorp/terraform:1.9` image.

**Resolved (2026-08-22, later the same night)**: the project's own sandbox
image is now published, multi-arch (`linux/amd64` + `linux/arm64`), to a
public registry — `ghcr.io/asingh-x/iac-agent/sandbox:latest` — which
sidesteps both obstacles below entirely: any cluster with normal internet
egress can just pull it directly, no `kind load`, no in-namespace registry,
no corporate-registry approval needed. This is now `SandboxImage`'s actual
default (`internal/config/settings.go`). Verified for real against this
same cluster: `TestK8sJobExecutor_RealCommandAndUser` and
`TestK8sJobExecutor_RealFileStaging` both pass against
`TF_AGENT_K8S_TEST_IMAGE=ghcr.io/asingh-x/iac-agent/sandbox:latest`, and a
direct `kubectl run` confirmed all three tools intact and runnable in the
pulled image (`terraform version` → `v1.15.9`, `tflint --version` →
`0.64.0`, `checkov --version` → `3.3.13`, `id` → `uid=10001(sandbox)`). The
image was independently verified free of the corporate TLS-proxy root CA
used to build it (see "What's built" above) both locally and in what was
actually pushed.

What follows is the original obstacle writeup, kept for the record — it's
why a registry was the right call rather than the two paths tried first:

1. This particular `kind` cluster's nodes aren't local Docker containers
   reachable from the machine `tf-agent` was being developed on (the API
   server sits behind a remote-looking hostname) and the `kind` CLI wasn't
   installed locally, so `kind load docker-image` — the normal way to get a
   locally-built image into a kind cluster without a registry — had no
   local node containers to target. Separately, the locally-built image was
   `arm64` and these nodes are `amd64`; that half was solved with
   `docker buildx build --platform linux/amd64` (needed a corporate
   TLS-proxy root CA dropped into the already-existing, gitignored
   `docker/sandbox/certs/` directory — the exact mechanism this doc already
   described for corporate-proxy builds, just needed locating the cert).
2. Getting the resulting `amd64` image onto the cluster needs a registry
   the nodes can actually pull from. The one realistic candidate found (the
   org's own internal registry) was **not** pushed to — that's a real
   external side effect on shared infrastructure, and a decision for a
   human, not something to do unilaterally while verifying a feature. A
   throwaway in-namespace `registry:2` Pod was tried as a fully
   self-contained alternative (push it via `kubectl port-forward`, using
   `crane` rather than `docker push` — `docker push`'s concurrent blob
   uploads were unreliable through Docker Desktop's VM-to-host loopback
   forwarding; `crane`'s sequential pushes moved the full ~168MB image in
   about 25 seconds). It doesn't work as an image *source* for real pods,
   though: `kubelet`/`containerd` resolve image references using the
   **node's own DNS resolver**, not `CoreDNS` — a `*.svc.cluster.local`
   name is never visible to it — and even addressing the registry by its
   raw `ClusterIP` directly, `containerd` refused it with `server gave HTTP
   response to HTTPS client` (no insecure-registry exception configured on
   the nodes, and adding one needs node-level config this project has no
   access to, and shouldn't reach for on a shared cluster). Both of these
   are structural, not something to route around with more retries or a
   different local port.

`K8sJobExecutor.Image` was always a plain config value — this is exactly why
resolving it needed no code change, just publishing the image and pointing
the default at it.
