package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// Default resource limits applied to sandbox containers when a
// DockerExecutor is constructed without explicit overrides.
const (
	DefaultMemory = "512m"
	DefaultCPUs   = "1"
	DefaultImage  = "iac-agent-sandbox:latest"
)

// DockerExecutor implements Executor by running each command inside a
// fresh, locked-down container via the `docker` CLI (exec.CommandContext
// invoking `docker run ...`, the same shell-out pattern internal/tools/bash.go
// already uses — no Docker SDK dependency).
//
// Containment properties, all load-bearing (not isolation theater):
//   - --network=none: none of terraform validate / tflint / checkov need
//     network access for local validation.
//   - --cap-drop=ALL --security-opt=no-new-privileges: no Linux capabilities,
//     no privilege escalation via setuid binaries.
//   - --memory / --cpus: bounded resource usage.
//   - Runs as the non-root user baked into the sandbox image (no --user
//     override to root).
//   - --rm plus a --name + `docker rm -f` backstop: the container is cleaned
//     up even if the local docker CLI process is killed hard (e.g. context
//     cancellation), since --rm only fires on a clean docker CLI exit.
type DockerExecutor struct {
	// Image is the sandbox image to run (see docker/sandbox/Dockerfile).
	Image string
	// Memory is the Docker --memory limit, e.g. "512m".
	Memory string
	// CPUs is the Docker --cpus limit, e.g. "1".
	CPUs string
}

// NewDockerExecutor builds a DockerExecutor, filling in sane defaults for
// any empty field.
func NewDockerExecutor(image, memory, cpus string) *DockerExecutor {
	if image == "" {
		image = DefaultImage
	}
	if memory == "" {
		memory = DefaultMemory
	}
	if cpus == "" {
		cpus = DefaultCPUs
	}
	return &DockerExecutor{Image: image, Memory: memory, CPUs: cpus}
}

// Run executes name+args inside a fresh container, with workDir bind-mounted
// read-write at /workspace (and set as the container's working directory) —
// the same role workDir plays for the host-exec path this replaces (cmd.Dir).
//
// ctx's deadline/cancellation is honored: cancelling ctx kills the local
// `docker run` client process. Because killing that client process does not
// by itself stop the container running inside the Docker daemon, Run always
// issues a `docker rm -f <container>` afterward — on a clean exit this is a
// harmless no-op (the container is already gone via --rm), but it is the
// backstop that guarantees no container is ever leaked on a hard kill.
func (d *DockerExecutor) Run(ctx context.Context, workDir string, name string, args ...string) (string, string, error) {
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", "", fmt.Errorf("sandbox: resolve workDir: %w", err)
	}

	containerName, err := randomContainerName()
	if err != nil {
		return "", "", fmt.Errorf("sandbox: generate container name: %w", err)
	}

	image := d.Image
	if image == "" {
		image = DefaultImage
	}
	memory := d.Memory
	if memory == "" {
		memory = DefaultMemory
	}
	cpus := d.CPUs
	if cpus == "" {
		cpus = DefaultCPUs
	}

	dockerArgs := []string{
		"run", "--rm",
		"--name", containerName,
		"-v", absWorkDir + ":/workspace:rw",
		"-w", "/workspace",
		"--network=none",
		"--memory=" + memory,
		"--cpus=" + cpus,
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		image,
		name,
	}
	dockerArgs = append(dockerArgs, args...)

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()

	// Backstop cleanup — always attempt this, regardless of how Run exited.
	// Use a fresh, short-lived context so cleanup still happens even when
	// ctx itself is already cancelled or expired.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", containerName).Run()
	cancel()

	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdoutBuf.String(), stderrBuf.String(), fmt.Errorf("sandbox: command aborted (%v): %w", ctxErr, runErr)
		}
		return stdoutBuf.String(), stderrBuf.String(), runErr
	}
	return stdoutBuf.String(), stderrBuf.String(), nil
}

func randomContainerName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "iac-agent-sandbox-" + hex.EncodeToString(b), nil
}
