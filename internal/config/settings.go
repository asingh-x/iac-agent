package config

// Config is the top-level configuration structure loaded from
// ~/.tf-agent/config.toml and overridden by environment variables.
type Config struct {
	Provider    ProviderConfig    `toml:"provider"`
	Agent       AgentConfig       `toml:"agent"`
	Permissions PermissionsConfig `toml:"permissions"`
	Hooks       HooksConfig       `toml:"hooks"`
	Server      ServerConfig      `toml:"server"`
}

type ServerConfig struct {
	Port                int    `toml:"port"`
	PostgresURL         string `toml:"postgres_url"` // postgres://user:pass@host:5432/db?sslmode=disable
	QueueDriver         string `toml:"queue_driver"` // memory (default) | nats
	NatsURL             string `toml:"nats_url"`     // nats://host:4222
	LLMConcurrency      int    `toml:"llm_concurrency"`
	PerUserConcurrency  int    `toml:"per_user_concurrency"`
	QueueBuffer         int    `toml:"queue_buffer"`
	NATSMaxMsgs         int    `toml:"nats_max_msgs"`         // max total backlog across all named NATS queues sharing the TF_AGENT stream (backpressure); default 5000
	ShutdownGracePeriod int    `toml:"shutdown_grace_period"` // seconds; default 60. How long to wait for in-flight tasks to finish before force-cancelling on shutdown.
	StaleTaskMaxAge     int    `toml:"stale_task_max_age"`    // seconds; default 7200 (2 hours). Age-based reconciliation backstop (db.Store.FailTasksOlderThan, run periodically from cmd/server/main.go) for a task whose queue delivery is abandoned/redelivered until it exhausts the queue's max-delivery-attempts and never reaches a terminal DB status any other way. Deliberately well above Agent.MaxTaskDuration's own default (1800s / 30 minutes): that field bounds a task that IS running; this one is a last-resort net for a task that never got a fair shot at running at all, so it must never fire on a task that's merely taking a while.

	// SemaphoreAcquireTimeout bounds how long a dequeued task will wait for
	// an LLM concurrency slot (the global semaphore, and separately the
	// per-user one) before failing cleanly instead of blocking the queue
	// worker goroutine — and this task's NATS delivery heartbeat — forever.
	// Without a bound, a task stuck here never reaches the point where its
	// context.CancelFunc is registered in Runner.cancels, so it is also
	// unreachable by Shutdown's grace-period force-cancel path. Seconds;
	// default 300 (5 minutes) — long enough to ride out a brief burst of
	// load, short enough that a genuinely saturated server sheds queued work
	// instead of accumulating permanently-blocked goroutines.
	SemaphoreAcquireTimeout int `toml:"semaphore_acquire_timeout"`

	// Sandbox: ValidateSkill/SecurityScanSkill always run
	// terraform/tflint/checkov inside a container (internal/sandbox) rather
	// than shelling out directly on the host — this is mandatory, not
	// configurable. SandboxBackend/SandboxImage/... below select and
	// configure which sandbox.Executor implementation is used.
	//
	// SandboxImage defaults to the published ghcr.io reference (see
	// Defaults() below), not a bare local tag. This matters for the
	// Kubernetes backend specifically: an unqualified name like
	// "iac-agent-sandbox:latest" would resolve to Docker Hub
	// (docker.io/library/...) when a real cluster tries to pull it, which
	// doesn't exist there — a real image reference is required for that
	// backend to work at all. The Docker backend benefits too: `docker run`
	// pulls a fully-qualified reference automatically if it's not already
	// built locally, so this works out of the box without requiring
	// `make sandbox-build` first (a local build, or overriding this to a
	// local tag, still takes precedence via Docker's local cache).
	SandboxImage  string `toml:"sandbox_image"`
	SandboxMemory string `toml:"sandbox_memory"` // Docker --memory value, e.g. "512m"
	SandboxCPUs   string `toml:"sandbox_cpus"`   // Docker --cpus value, e.g. "1"

	// SandboxBackend selects which sandbox.Executor implementation is wired
	// up: "docker" (default) runs sandbox.DockerExecutor against a local
	// Docker daemon; "kubernetes" runs sandbox.K8sJobExecutor against a real
	// cluster (see docs/sandbox.md). Any other value falls back to "docker".
	SandboxBackend string `toml:"sandbox_backend"`
	// SandboxKubeNamespace is the (pre-existing — K8sJobExecutor never
	// creates it) namespace K8sJobExecutor creates its Jobs in.
	SandboxKubeNamespace string `toml:"sandbox_kube_namespace"`
	// SandboxKubeconfigPath, if set, is passed to K8sJobExecutor instead of
	// the standard client-go kubeconfig resolution order ($KUBECONFIG, then
	// ~/.kube/config, then in-cluster config when running inside a pod).
	SandboxKubeconfigPath string `toml:"sandbox_kubeconfig_path"`
	// SandboxKubeMemory/SandboxKubeCPUs are Kubernetes resource.Quantity
	// strings (e.g. "512Mi", "1") — not the same syntax as
	// SandboxMemory/SandboxCPUs, which are Docker --memory/--cpus values.
	SandboxKubeMemory string `toml:"sandbox_kube_memory"`
	SandboxKubeCPUs   string `toml:"sandbox_kube_cpus"`
}

type ProviderConfig struct {
	Name      string          `toml:"name"`
	Model     string          `toml:"model"`
	Anthropic AnthropicConfig `toml:"anthropic"`
	Bedrock   BedrockConfig   `toml:"bedrock"`
}

type AnthropicConfig struct {
	APIKey string `toml:"api_key"`
}

type BedrockConfig struct {
	Region string `toml:"region"`
	Model  string `toml:"model"`
}

type AgentConfig struct {
	MaxTurns            int  `toml:"max_turns"`
	MaxTokens           int  `toml:"max_tokens"`
	Debug               bool `toml:"debug"`
	WaitForInputTimeout int  `toml:"wait_for_input_timeout"` // seconds; default 604800 (7 days)
	MaxTaskDuration     int  `toml:"max_task_duration"`      // seconds; default 1800 (30 minutes)
}

type PermissionsConfig struct {
	Bash    string `toml:"bash"`
	Write   string `toml:"write"`
	Edit    string `toml:"edit"`
	Read    string `toml:"read"`
	Glob    string `toml:"glob"`
	Grep    string `toml:"grep"`
	Ls      string `toml:"ls"`
	Default string `toml:"default"`
}

type HooksConfig struct {
	PreToolUse  []HookEntry `toml:"pre_tool_use"`
	PostToolUse []HookEntry `toml:"post_tool_use"`
}

type HookEntry struct {
	Tool    string `toml:"tool"`
	Command string `toml:"command"`
}

// Defaults returns a Config with sane default values.
func Defaults() *Config {
	return &Config{
		Provider: ProviderConfig{
			Name:  "anthropic",
			Model: "claude-sonnet-4-6",
			Bedrock: BedrockConfig{
				Region: "us-east-1",
				Model:  "us.anthropic.claude-sonnet-4-6-20251101-v1:0",
			},
		},
		Agent: AgentConfig{
			MaxTurns:            30,
			MaxTokens:           8192,
			Debug:               false,
			WaitForInputTimeout: 7 * 24 * 3600, // 7 days in seconds
			MaxTaskDuration:     30 * 60,       // 30 minutes in seconds
		},
		Server: ServerConfig{
			Port:                    8080,
			LLMConcurrency:          10,
			PerUserConcurrency:      3,
			QueueBuffer:             500,
			NATSMaxMsgs:             5000, // NATS is durable and meant to hold more backlog than the in-memory QueueBuffer (500); keep in sync with queue.DefaultNATSMaxMsgs
			ShutdownGracePeriod:     60,
			StaleTaskMaxAge:         2 * 60 * 60, // 2 hours in seconds; ~4x Agent.MaxTaskDuration's own 30-minute default, see field comment
			SemaphoreAcquireTimeout: 5 * 60,      // 5 minutes in seconds, see field comment
			SandboxImage:            "ghcr.io/asingh-x/iac-agent/sandbox:latest",
			SandboxMemory:           "512m",
			SandboxCPUs:             "1",
			SandboxBackend:          "docker",
			SandboxKubeNamespace:    "iac-agent-sandbox",
			SandboxKubeMemory:       "512Mi",
			SandboxKubeCPUs:         "1",
		},
		Permissions: PermissionsConfig{
			// The product's review gate is the PR, not per-tool-call
			// confirmation: everything from repo scan through validate,
			// security scan, and file writes runs autonomously, and the
			// human reviews the actual diff at the PR — same as any other
			// GitOps change. write/edit are already path-scoped to the
			// task's working directory (escapes are rejected). bash runs on
			// the host unconfirmed under this default (BashTool has no
			// sandbox integration); override bash to "ask"/"confirm" here
			// if you want a manual gate before shell commands run.
			Bash:    "auto",
			Write:   "auto",
			Edit:    "auto",
			Read:    "auto",
			Glob:    "auto",
			Grep:    "auto",
			Ls:      "auto",
			Default: "auto",
		},
	}
}
