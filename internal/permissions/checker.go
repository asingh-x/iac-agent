package permissions

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/tf-agent/tf-agent/internal/config"
)

// Checker evaluates permission levels for tool invocations.
type Checker struct {
	perTool map[string]Level
	def     Level
	session map[string]Level // session-level overrides set via interactive prompts
}

// normalizeLevel maps a config string to a Level, accepting "confirm" as a
// documented alias for "ask" so config.sample.toml / README examples using
// either spelling behave identically.
func normalizeLevel(s string) Level {
	if s == "confirm" {
		return LevelAsk
	}
	return Level(s)
}

// NewChecker creates a Checker from config.
func NewChecker(cfg *config.PermissionsConfig) *Checker {
	perTool := map[string]Level{
		"bash":  normalizeLevel(cfg.Bash),
		"write": normalizeLevel(cfg.Write),
		"edit":  normalizeLevel(cfg.Edit),
		"read":  normalizeLevel(cfg.Read),
		"glob":  normalizeLevel(cfg.Glob),
		"grep":  normalizeLevel(cfg.Grep),
		"ls":    normalizeLevel(cfg.Ls),
	}
	def := LevelAsk
	if cfg.Default != "" {
		def = normalizeLevel(cfg.Default)
	}
	return &Checker{
		perTool: perTool,
		def:     def,
		session: make(map[string]Level),
	}
}

// Check returns the permission level for the named tool. Session overrides
// (set via Prompt) take precedence over config values.
func (c *Checker) Check(toolName string, _ json.RawMessage) Level {
	// Session overrides take precedence.
	if l, ok := c.session[toolName]; ok {
		return l
	}

	if l, ok := c.perTool[toolName]; ok && l != "" {
		return l
	}
	return c.def
}

// Prompt asks the user interactively on stderr/stdin whether to allow the
// named tool to run. It returns the resulting Level and may record a
// session-level override (always/never) for future calls.
//
// Accepted answers:
//   - "y" or enter  → allow once (LevelAuto, not persisted)
//   - "a"           → allow always (sets session override to LevelAuto)
//   - "n"           → deny once (LevelDeny, not persisted)
//   - "N"           → deny always (sets session override to LevelDeny)
func (c *Checker) Prompt(toolName, preview string) Level {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Fprintf(os.Stderr,
			"\n[permission] Allow tf-agent to run %s: `%s`?  [y]es / [n]o / [a]lways / [N]ever: ",
			toolName, preview)

		line, err := reader.ReadString('\n')
		if err != nil {
			// stdin closed or error — default to deny.
			return LevelDeny
		}
		answer := strings.TrimSpace(line)

		switch answer {
		case "y", "yes", "":
			return LevelAuto // allow once
		case "a", "always":
			c.session[toolName] = LevelAuto
			return LevelAuto
		case "n", "no":
			return LevelDeny // deny once
		case "N", "Never":
			c.session[toolName] = LevelDeny
			return LevelDeny
		default:
			fmt.Fprintln(os.Stderr, "Please enter y, n, a, or N.")
		}
	}
}
