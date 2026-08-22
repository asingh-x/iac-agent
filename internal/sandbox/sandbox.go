// Package sandbox provides an isolated execution boundary for running
// untrusted-input CLI tools (terraform, tflint, checkov) that today run
// directly on the host process via os/exec (see internal/skills/validate.go
// and internal/skills/security_scan.go).
//
// The Executor interface is the extension point. DockerExecutor runs each
// command in a local Docker container (single node). K8sJobExecutor runs
// each command as a Kubernetes Job's pod, for multi-node production
// deployments — see docs/SANDBOX.md for what was verified against a real
// cluster, including the exec/tar file-staging approach it uses and the
// NetworkPolicy enforcement caveat: not every CNI enforces it, so confirm
// it for your own cluster rather than assuming — this project's own kind
// test cluster's kindnetd build does enforce it, verified for real
// 2026-08-22 (see docs/SANDBOX.md).
package sandbox

import "context"

// Executor runs a command against a working directory and returns its
// stdout and stderr separately (callers that want the old combined-output
// behavior can concatenate them at the call site — see
// internal/skills/validate.go). name/args mean exactly what they mean for
// os/exec.CommandContext (and, before this package existed, what they meant
// to internal/skills' runCmd helper) — this is a drop-in replacement for
// that calling convention, not a new one.
type Executor interface {
	Run(ctx context.Context, workDir string, name string, args ...string) (stdout string, stderr string, err error)
}
