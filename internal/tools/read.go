package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ReadTool reads a file with line numbers (cat -n style).
type ReadTool struct {
	cwd string
}

func NewReadTool(cwd string) *ReadTool { return &ReadTool{cwd: cwd} }

func (t *ReadTool) Name() string                         { return "read" }
func (t *ReadTool) IsReadOnly() bool                     { return true }
func (t *ReadTool) IsDestructive(_ json.RawMessage) bool { return false }

func (t *ReadTool) Description() string {
	return "Read a file and return its contents with line numbers. Supports offset and limit parameters for large files."
}

func (t *ReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "Absolute or relative path to the file to read"
			},
			"offset": {
				"type": "integer",
				"description": "Line number to start reading from (1-based)"
			},
			"limit": {
				"type": "integer",
				"description": "Maximum number of lines to read"
			}
		},
		"required": ["file_path"]
	}`)
}

func (t *ReadTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("read: invalid input: %w", err)
	}
	if args.FilePath == "" {
		return "", fmt.Errorf("read: file_path is required")
	}

	path, err := resolveScoped(t.cwd, args.FilePath)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	lineNum := 0
	written := 0

	for scanner.Scan() {
		lineNum++
		if args.Offset > 0 && lineNum < args.Offset {
			continue
		}
		if args.Limit > 0 && written >= args.Limit {
			break
		}
		fmt.Fprintf(&sb, "%d\t%s\n", lineNum, scanner.Text())
		written++
	}
	if err := scanner.Err(); err != nil {
		return sb.String(), fmt.Errorf("read: %w", err)
	}
	return sb.String(), nil
}
