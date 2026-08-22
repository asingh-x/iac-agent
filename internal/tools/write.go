package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteTool writes content to a file.
type WriteTool struct {
	cwd string
}

func NewWriteTool(cwd string) *WriteTool { return &WriteTool{cwd: cwd} }

func (t *WriteTool) Name() string                         { return "write" }
func (t *WriteTool) IsReadOnly() bool                     { return false }
func (t *WriteTool) IsDestructive(_ json.RawMessage) bool { return true }

func (t *WriteTool) Description() string {
	return "Write content to a file, creating it if it doesn't exist or overwriting if it does."
}

func (t *WriteTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "Path to the file to write"
			},
			"content": {
				"type": "string",
				"description": "Content to write to the file"
			}
		},
		"required": ["file_path", "content"]
	}`)
}

// blockedFilePatterns lists filename patterns that must not be written.
var blockedFilePatterns = []string{
	".gitconfig", ".zshrc", ".bashrc", ".env",
	".pem", ".key", ".p12", ".pfx",
}

func isBlockedPath(path string) bool {
	base := filepath.Base(path)
	for _, pattern := range blockedFilePatterns {
		if strings.HasSuffix(base, pattern) || base == pattern {
			return true
		}
	}
	return false
}

func (t *WriteTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("write: invalid input: %w", err)
	}
	if args.FilePath == "" {
		return "", fmt.Errorf("write: file_path is required")
	}

	if isBlockedPath(args.FilePath) {
		return "", fmt.Errorf("write: writing to %q is not allowed", args.FilePath)
	}

	path, err := resolveScoped(t.cwd, args.FilePath)
	if err != nil {
		return "", fmt.Errorf("write: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", fmt.Errorf("write: mkdir: %w", err)
	}

	if err := os.WriteFile(path, []byte(args.Content), 0644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return fmt.Sprintf("File written: %s (%d bytes)", path, len(args.Content)), nil
}
