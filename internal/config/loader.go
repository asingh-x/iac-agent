package config

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ConfigDir returns the path to ~/.tf-agent.
func ConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tf-agent")
}

// ConfigPath returns the path to the config file.
func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.toml")
}

// Load reads the config file (if present) and applies environment variable
// overrides. Missing file is not an error — defaults are used.
func Load() (*Config, error) {
	cfg := Defaults()

	path := ConfigPath()
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, err
		}
	}

	// Environment variable overrides.
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		cfg.Provider.Anthropic.APIKey = v
	}
	if v := os.Getenv("CLAURST_PROVIDER"); v != "" {
		cfg.Provider.Name = v
	}
	if v := os.Getenv("CLAURST_MODEL"); v != "" {
		cfg.Provider.Model = v
	}
	if v := os.Getenv("TF_AGENT_DEBUG"); v == "true" || v == "1" {
		cfg.Agent.Debug = true
	}
	if v := os.Getenv("TF_AGENT_SANDBOX_ENABLED"); v == "true" || v == "1" {
		cfg.Server.SandboxEnabled = true
	}
	if v := os.Getenv("TF_AGENT_SANDBOX_IMAGE"); v != "" {
		cfg.Server.SandboxImage = v
	}
	if v := os.Getenv("TF_AGENT_SANDBOX_BACKEND"); v != "" {
		cfg.Server.SandboxBackend = v
	}
	if v := os.Getenv("TF_AGENT_SANDBOX_KUBE_NAMESPACE"); v != "" {
		cfg.Server.SandboxKubeNamespace = v
	}
	if v := os.Getenv("TF_AGENT_SANDBOX_KUBECONFIG_PATH"); v != "" {
		cfg.Server.SandboxKubeconfigPath = v
	}

	// Ensure config dir exists for session storage.
	_ = os.MkdirAll(filepath.Join(ConfigDir(), "sessions"), 0755)

	return cfg, nil
}
