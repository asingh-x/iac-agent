package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// EditTool performs exact string replacement in a file.
type EditTool struct {
	cwd string
}

func NewEditTool(cwd string) *EditTool { return &EditTool{cwd: cwd} }

func (t *EditTool) Name() string                         { return "edit" }
func (t *EditTool) IsReadOnly() bool                     { return false }
func (t *EditTool) IsDestructive(_ json.RawMessage) bool { return true }

func (t *EditTool) Description() string {
	return "Replace an exact string in a file. old_string must be unique in the file. Use read first to see the current contents."
}

func (t *EditTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "Path to the file to edit"
			},
			"old_string": {
				"type": "string",
				"description": "The exact string to replace (must be unique in the file)"
			},
			"new_string": {
				"type": "string",
				"description": "The replacement string"
			},
			"replace_all": {
				"type": "boolean",
				"description": "Replace all occurrences (default false)"
			}
		},
		"required": ["file_path", "old_string", "new_string"]
	}`)
}

func (t *EditTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("edit: invalid input: %w", err)
	}
	if args.FilePath == "" {
		return "", fmt.Errorf("edit: file_path is required")
	}
	if isBlockedPath(args.FilePath) {
		return "", fmt.Errorf("edit: editing %q is not allowed", args.FilePath)
	}

	path, err := resolveScoped(t.cwd, args.FilePath)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("edit: read: %w", err)
	}

	content := string(data)
	if !strings.Contains(content, args.OldString) {
		return "", fmt.Errorf("edit: old_string not found in file")
	}

	count := strings.Count(content, args.OldString)
	if !args.ReplaceAll && count > 1 {
		return "", fmt.Errorf("edit: old_string appears %d times — use replace_all:true or provide more context", count)
	}

	var newContent string
	if args.ReplaceAll {
		newContent = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		newContent = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("edit: write: %w", err)
	}

	replacements := 1
	if args.ReplaceAll {
		replacements = count
	}
	return fmt.Sprintf("Edited %s: replaced %d occurrence(s)", path, replacements), nil
}
