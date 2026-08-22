package config

import "testing"

func TestDefaults_DestructiveToolsRequireConfirmation(t *testing.T) {
	cfg := Defaults()

	destructive := map[string]string{
		"bash":  cfg.Permissions.Bash,
		"write": cfg.Permissions.Write,
		"edit":  cfg.Permissions.Edit,
	}
	for tool, level := range destructive {
		if level == "auto" {
			t.Errorf("destructive tool %q defaults to %q — must require confirmation out of the box", tool, level)
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
