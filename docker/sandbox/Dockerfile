# Sandbox image for internal/sandbox.DockerExecutor — the container that
# terraform/tflint/checkov actually run inside of when
# server.sandbox_enabled = true (see docs/sandbox.md).
#
# This is NOT the iac-agent server image (see the top-level Dockerfile for
# that). This image never runs the server; it only holds the three CLI
# tools ValidateSkill/SecurityScanSkill invoke, plus a non-root user to run
# them as. The container is always started with --network=none,
# --cap-drop=ALL, --security-opt=no-new-privileges, and --memory/--cpus
# limits by DockerExecutor — this image supplies no isolation on its own.
#
# Build: make sandbox-build
#   (docker build -f docker/sandbox/Dockerfile -t iac-agent-sandbox:latest .)

# ── Stage 1: fetch terraform + tflint release binaries ───────────────────────
FROM debian:bookworm-slim AS fetcher

ARG TERRAFORM_VERSION=1.15.9
ARG TFLINT_VERSION=0.64.0
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      unzip \
    && rm -rf /var/lib/apt/lists/*

# Optional: trust an extra CA (e.g. a corporate TLS-inspecting proxy) when
# building behind one. Empty by default (docker/sandbox/certs/ holds only
# .gitkeep) so this is a no-op for a normal/CI build — it only ever ADDS
# trust for a cert you explicitly drop in that directory, it never disables
# verification. See docker/sandbox/certs/README.
COPY docker/sandbox/certs/ /usr/local/share/ca-certificates/extra/
RUN update-ca-certificates

RUN curl -fsSL -o /tmp/terraform.zip \
      "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${TARGETARCH}.zip" \
    && unzip -q /tmp/terraform.zip -d /out \
    && rm /tmp/terraform.zip \
    && chmod +x /out/terraform

RUN curl -fsSL -o /tmp/tflint.zip \
      "https://github.com/terraform-linters/tflint/releases/download/v${TFLINT_VERSION}/tflint_linux_${TARGETARCH}.zip" \
    && unzip -q /tmp/tflint.zip -d /out \
    && rm /tmp/tflint.zip \
    && chmod +x /out/tflint

# ── Stage 2: final sandbox image ─────────────────────────────────────────────
# python:3.12-slim gives checkov a glibc target with manylinux wheels
# available for its dependencies, so no compiler toolchain is needed here.
FROM python:3.12-slim

ARG CHECKOV_VERSION=3.3.13

COPY --from=fetcher /out/terraform /usr/local/bin/terraform
COPY --from=fetcher /out/tflint /usr/local/bin/tflint

RUN pip install --no-cache-dir "checkov==${CHECKOV_VERSION}"

# Non-root user the sandbox always runs as (DockerExecutor never passes
# --user, so this image's default USER is what actually takes effect).
RUN useradd --create-home --uid 10001 --shell /usr/sbin/nologin sandbox

USER sandbox
WORKDIR /workspace

# No ENTRYPOINT/CMD: DockerExecutor.Run supplies the full command
# (terraform/tflint/checkov + args) as the `docker run` command.
