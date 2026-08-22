package llm

import (
	"fmt"

	"github.com/tf-agent/tf-agent/internal/config"
)

// NewProvider constructs the correct Provider implementation from config.
func NewProvider(cfg *config.Config) (Provider, error) {
	debug := cfg.Agent.Debug

	switch cfg.Provider.Name {
	case "anthropic", "":
		key := cfg.Provider.Anthropic.APIKey
		if key == "" {
			return nil, fmt.Errorf(
				"ANTHROPIC_API_KEY is not set — set it via environment variable or config.toml",
			)
		}
		return NewCircuitBreakerProvider(NewAnthropicProvider(key, debug)), nil

	case "bedrock":
		region := cfg.Provider.Bedrock.Region
		if region == "" {
			region = "us-east-1"
		}
		model := cfg.Provider.Bedrock.Model
		if model == "" {
			model = "us.anthropic.claude-sonnet-4-6-20251101-v1:0"
		}
		p, err := NewBedrockProvider(region, model, debug)
		if err != nil {
			return nil, err
		}
		return NewCircuitBreakerProvider(p), nil

	default:
		return nil, fmt.Errorf("unknown provider %q — choose 'anthropic' or 'bedrock'", cfg.Provider.Name)
	}
}

// ModelName returns the model string to use for API calls.
func ModelName(cfg *config.Config) string {
	if cfg.Provider.Name == "bedrock" {
		m := cfg.Provider.Bedrock.Model
		if m != "" {
			return m
		}
		return "us.anthropic.claude-sonnet-4-6-20251101-v1:0"
	}
	m := cfg.Provider.Model
	if m != "" {
		return m
	}
	return "claude-sonnet-4-6"
}
