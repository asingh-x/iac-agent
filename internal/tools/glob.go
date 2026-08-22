package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// GlobTool finds files by glob pattern.
type GlobTool struct {
	cwd string
}

func NewGlobTool(cwd string) *GlobTool { return &GlobTool{cwd: cwd} }

func (t *GlobTool) Name() string                         { return "glob" }
func (t *GlobTool) IsReadOnly() bool                     { return true }
func (t *GlobTool) IsDestructive(_ json.RawMessage) bool { return false }

func (t *GlobTool) Description() string {
	return "Find files matching a glob pattern. Returns matching file paths sorted by modification time."
}

func (t *GlobTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {
				"type": "string",
				"description": "Glob pattern to match files against (e.g. '**/*.go', 'src/**/*.ts')"
			},
			"path": {
				"type": "string",
				"description": "Directory to search in (default: current working directory)"
			}
		},
		"required": ["pattern"]
	}`)
}

func (t *GlobTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("glob: invalid input: %w", err)
	}
	if args.Pattern == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}

	searchDir := t.cwd
	if args.Path != "" {
		var err error
		searchDir, err = resolveScoped(t.cwd, args.Path)
		if err != nil {
			return "", fmt.Errorf("glob: %w", err)
		}
	}

	fullPattern := filepath.Join(searchDir, args.Pattern)
	matches, err := filepath.Glob(fullPattern)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}

	// Make paths relative to cwd where possible, dropping any match that
	// escapes the working directory via a ".."-containing pattern.
	result := make([]string, 0, len(matches))
	for _, m := range matches {
		if !withinScope(t.cwd, m) {
			continue
		}
		if rel, err := filepath.Rel(t.cwd, m); err == nil {
			result = append(result, rel)
		} else {
			result = append(result, m)
		}
	}

	if len(result) == 0 {
		return "No files found matching pattern", nil
	}

	return strings.Join(result, "\n"), nil
}
