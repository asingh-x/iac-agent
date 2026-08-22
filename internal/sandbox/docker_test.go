package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tf-agent/tf-agent/internal/sandbox"
)

// testImage is the sandbox image built by `make sandbox-build` from
// docker/sandbox/Dockerfile. Tests that need it skip cleanly if it hasn't
// been built, so `go test ./...` stays green for anyone who has Docker but
// hasn't opted into the sandbox yet.
const testImage = "iac-agent-sandbox:latest"

// requireDocker skips the test if a Docker daemon or the sandbox image
// isn't available — mirrors this repo's existing NATS_URL-gated skip
// pattern (internal/queue/nats_test.go) for environment-dependent tests.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed — skipping sandbox integration test")
	}
	if err := exec.Command("docker", "image", "inspect", testImage).Run(); err != nil {
		t.Skipf("sandbox image %s not built — run 'make sandbox-build' to enable this test", testImage)
	}
}

func TestNewDockerExecutor_Defaults(t *testing.T) {
	e := sandbox.NewDockerExecutor("", "", "")
	if e.Image != sandbox.DefaultImage {
		t.Errorf("Image = %q, want default %q", e.Image, sandbox.DefaultImage)
	}
	if e.Memory != sandbox.DefaultMemory {
		t.Errorf("Memory = %q, want default %q", e.Memory, sandbox.DefaultMemory)
	}
	if e.CPUs != sandbox.DefaultCPUs {
		t.Errorf("CPUs = %q, want default %q", e.CPUs, sandbox.DefaultCPUs)
	}
}

func TestNewDockerExecutor_CustomValues(t *testing.T) {
	e := sandbox.NewDockerExecutor("custom:tag", "1g", "2")
	if e.Image != "custom:tag" || e.Memory != "1g" || e.CPUs != "2" {
		t.Errorf("got %+v, want custom values preserved", e)
	}
}

// TestDockerExecutor_RealTerraformValidate proves the whole plumbing works:
// a real `terraform init` + `terraform validate` against a trivial fixture,
// run inside the sandbox container, producing real output.
func TestDockerExecutor_RealTerraformValidate(t *testing.T) {
	requireDocker(t)

	dir := t.TempDir()
	// Deliberately provider-free (no `resource` blocks) so `terraform init`
	// needs no network access — proving validate works standalone, without
	// relying on --network=none being bypassed for a provider download.
	tf := "terraform {\n  required_version = \">= 1.0\"\n}\n\n" +
		"variable \"greeting\" {\n  type    = string\n  default = \"hello\"\n}\n\n" +
		"output \"greeting\" {\n  value = var.greeting\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	de := sandbox.NewDockerExecutor(testImage, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if stdout, stderr, err := de.Run(ctx, dir, "terraform", "init", "-backend=false", "-no-color"); err != nil {
		t.Fatalf("terraform init failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	stdout, stderr, err := de.Run(ctx, dir, "terraform", "validate", "-no-color")
	if err != nil {
		t.Fatalf("terraform validate failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Success") {
		t.Errorf("expected terraform validate success output, got:\n%s", stdout)
	}
}

// TestDockerExecutor_RealTflint proves tflint runs for real inside the
// sandbox and returns real output.
func TestDockerExecutor_RealTflint(t *testing.T) {
	requireDocker(t)

	dir := t.TempDir()
	tf := "resource \"null_resource\" \"example\" {}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	de := sandbox.NewDockerExecutor(testImage, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// tflint exits non-zero when it reports issues (same as it would running
	// directly on the host) — that's expected, not a sandbox failure, so we
	// only assert on the real JSON output shape, not the exit code.
	stdout, _, _ := de.Run(ctx, dir, "tflint", "--format=json")
	if !strings.Contains(stdout, `"issues"`) {
		t.Errorf("expected tflint json output containing \"issues\", got:\n%s", stdout)
	}
}

// TestDockerExecutor_NetworkNoneBlocksNetwork proves --network=none is a
// real containment property and not isolation theater: an outbound
// connection attempt from inside the container must fail.
func TestDockerExecutor_NetworkNoneBlocksNetwork(t *testing.T) {
	requireDocker(t)

	dir := t.TempDir()
	de := sandbox.NewDockerExecutor(testImage, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stdout, stderr, err := de.Run(ctx, dir, "python3", "-c",
		"import socket; socket.create_connection(('8.8.8.8', 53), timeout=5)")
	if err == nil {
		t.Fatalf("expected outbound connection to fail under --network=none, but it succeeded; stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}

// TestDockerExecutor_CancelKillsContainerNoLeak proves that cancelling ctx
// actually kills the container (not just the local `docker run` client
// process) — no container should be left running afterward.
func TestDockerExecutor_CancelKillsContainerNoLeak(t *testing.T) {
	requireDocker(t)

	dir := t.TempDir()
	de := sandbox.NewDockerExecutor(testImage, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err := de.Run(ctx, dir, "python3", "-c", "import time; time.sleep(60)")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected Run to return an error for a cancelled/timed-out command")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("Run took %s to return after a 2s context timeout — cleanup is not prompt", elapsed)
	}

	// Give the daemon a moment to settle, then confirm no leaked container.
	time.Sleep(500 * time.Millisecond)
	psOut, psErr := exec.Command("docker", "ps", "--filter", "name=iac-agent-sandbox-", "--format", "{{.Names}}").Output()
	if psErr != nil {
		t.Fatalf("docker ps failed: %v", psErr)
	}
	if leftover := strings.TrimSpace(string(psOut)); leftover != "" {
		t.Fatalf("container(s) leaked after cancellation: %q", leftover)
	}
}
