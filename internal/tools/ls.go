package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// LsTool lists directory contents.
type LsTool struct {
	cwd string
}

func NewLsTool(cwd string) *LsTool { return &LsTool{cwd: cwd} }

func (t *LsTool) Name() string                         { return "ls" }
func (t *LsTool) IsReadOnly() bool                     { return true }
func (t *LsTool) IsDestructive(_ json.RawMessage) bool { return false }

func (t *LsTool) Description() string {
	return "List directory contents. Returns file and directory names with basic info."
}

func (t *LsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Directory path to list (default: current working directory)"
			}
		}
	}`)
}

func (t *LsTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &args)

	dir := t.cwd
	if args.Path != "" {
		var err error
		dir, err = resolveScoped(t.cwd, args.Path)
		if err != nil {
			return "", fmt.Errorf("ls: %w", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("ls: %w", err)
	}

	var sb strings.Builder
	for _, e := range entries {
		info, _ := e.Info()
		if e.IsDir() {
			fmt.Fprintf(&sb, "%s/\n", e.Name())
		} else if info != nil {
			fmt.Fprintf(&sb, "%s (%d bytes)\n", e.Name(), info.Size())
		} else {
			fmt.Fprintf(&sb, "%s\n", e.Name())
		}
	}
	if sb.Len() == 0 {
		return "(empty directory)", nil
	}
	return sb.String(), nil
}
