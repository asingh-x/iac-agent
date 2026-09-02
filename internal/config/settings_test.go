package config

import "testing"

func TestDefaults_AllToolsAutoByDefault(t *testing.T) {
	// The review gate is the PR, not a per-tool-call prompt: everything
	// through repo scan, generate, validate, security scan, and file writes
	// runs autonomously by default. write/edit are path-scoped to the
	// task's working directory; bash runs on the host under this default
	// unless the operator overrides bash back to "ask"/"confirm" in their
	// own config.
	cfg := Defaults()

	autoByDefault := map[string]string{
		"bash":  cfg.Permissions.Bash,
		"write": cfg.Permissions.Write,
		"edit":  cfg.Permissions.Edit,
	}
	for tool, level := range autoByDefault {
		if level != "auto" {
			t.Errorf("tool %q defaults to %q, want auto — the PR is the review gate, not a per-tool-call prompt", tool, level)
		}
	}

	readOnly := map[string]string{
		"read": cfg.Permissions.Read,
		"glob": cfg.Permissions.Glob,
		"grep": cfg.Permissions.Grep,
		"ls":   cfg.Permissions.Ls,
	}
	for tool, level := range readOnly {
		if level != "auto" {
			t.Errorf("read-only tool %q defaults to %q, want auto", tool, level)
		}
	}
}

func TestDefaults_NATSMaxMsgsIsSanePositiveDefault(t *testing.T) {
	// NATSMaxMsgs bounds the shared JetStream stream's backlog for NATSQueue
	// backpressure (see queue.NewNATSQueue / queue.DefaultNATSMaxMsgs). It
	// must default to a sane positive value out of the box — zero or
	// negative would either disable the stream limit or make AddStream
	// reject the config outright, and the value should be larger than the
	// in-memory QueueBuffer default since NATS is durable and meant to
	// absorb more backlog.
	cfg := Defaults()
	if cfg.Server.NATSMaxMsgs <= 0 {
		t.Fatalf("Server.NATSMaxMsgs = %d, want a positive default", cfg.Server.NATSMaxMsgs)
	}
	if cfg.Server.NATSMaxMsgs < cfg.Server.QueueBuffer {
		t.Errorf("Server.NATSMaxMsgs = %d is smaller than QueueBuffer = %d; NATS is durable and should hold at least as much backlog as the in-memory queue", cfg.Server.NATSMaxMsgs, cfg.Server.QueueBuffer)
	}
}

func TestDefaults_StaleTaskMaxAgeIsWellAboveMaxTaskDuration(t *testing.T) {
	// StaleTaskMaxAge (used by the age-based reconciliation backstop, see
	// cmd/server's runStaleTaskReconciler / db.Store.FailTasksOlderThan) must
	// default to comfortably more than Agent.MaxTaskDuration: that field
	// bounds a task that IS actively running; this one is a last-resort net
	// for a task that never even got a fair shot at running (e.g. NATS
	// delivery abandoned/redelivered until MaxDeliver is exhausted). If this
	// ever regressed to being close to or smaller than MaxTaskDuration, the
	// reconciler could fail a task that is still well within its normal
	// execution budget.
	cfg := Defaults()
	if cfg.Server.StaleTaskMaxAge <= 0 {
		t.Fatalf("Server.StaleTaskMaxAge = %d, want a positive default", cfg.Server.StaleTaskMaxAge)
	}
	if cfg.Server.StaleTaskMaxAge < 3*cfg.Agent.MaxTaskDuration {
		t.Errorf("Server.StaleTaskMaxAge = %d is not comfortably larger than Agent.MaxTaskDuration = %d (want at least 3x)", cfg.Server.StaleTaskMaxAge, cfg.Agent.MaxTaskDuration)
	}
}

func TestDefaults_FallbackStaysAuto(t *testing.T) {
	// "default" is the fallback for every skill (repo_scan, generate_terraform,
	// CreatePR, ...) and non-file tool (ask_user, agent, task, web_fetch,
	// web_search) — none of those are in the per-tool permission list, so if
	// this regresses to "ask" the entire autonomous pipeline stalls waiting
	// on a human at every single skill call.
	cfg := Defaults()
	if cfg.Permissions.Default != "auto" {
		t.Errorf("Permissions.Default = %q, want auto", cfg.Permissions.Default)
	}
}

// TestDefaults_SandboxConfigHasSaneDefaults guards the sandbox.Executor
// config fields' defaults — sandboxed execution is mandatory (see
// internal/server/task_runner.go's wireAgent), so these must always resolve
// to something usable out of the box, with no opt-out.
func TestDefaults_SandboxConfigHasSaneDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Server.SandboxImage == "" {
		t.Error("SandboxImage should have a non-empty default")
	}
	if cfg.Server.SandboxMemory == "" {
		t.Error("SandboxMemory should have a non-empty default")
	}
	if cfg.Server.SandboxCPUs == "" {
		t.Error("SandboxCPUs should have a non-empty default")
	}
	if cfg.Server.SandboxBackend != "docker" {
		t.Errorf("SandboxBackend = %q, want default %q", cfg.Server.SandboxBackend, "docker")
	}
	if cfg.Server.SandboxKubeNamespace == "" {
		t.Error("SandboxKubeNamespace should have a non-empty default")
	}
	if cfg.Server.SandboxKubeMemory == "" {
		t.Error("SandboxKubeMemory should have a non-empty default")
	}
	if cfg.Server.SandboxKubeCPUs == "" {
		t.Error("SandboxKubeCPUs should have a non-empty default")
	}
}
