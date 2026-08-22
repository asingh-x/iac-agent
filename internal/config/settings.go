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
			Port:                8080,
			LLMConcurrency:      10,
			PerUserConcurrency:  3,
			QueueBuffer:         500,
			NATSMaxMsgs:         5000, // NATS is durable and meant to hold more backlog than the in-memory QueueBuffer (500); keep in sync with queue.DefaultNATSMaxMsgs
			ShutdownGracePeriod: 60,
		},
		Permissions: PermissionsConfig{
			// Destructive tools (mutate the filesystem or run arbitrary shell
			// commands) require confirmation by default. Read-only tools stay
			// auto so exploration doesn't need a human in the loop. Default
			// stays "auto" — it's the fallback for every skill (repo_scan,
			// generate_terraform, CreatePR, ...) and other non-file tools
			// (ask_user, agent, task, web_fetch, web_search), none of which
			// are in the per-tool list above; "ask" here would stall the
			// normal autonomous pipeline at every single skill invocation.
			Bash:    "ask",
			Write:   "ask",
			Edit:    "ask",
			Read:    "auto",
			Glob:    "auto",
			Grep:    "auto",
			Ls:      "auto",
			Default: "auto",
		},
	}
}
