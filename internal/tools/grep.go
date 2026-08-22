package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GrepTool searches file contents using regex.
type GrepTool struct {
	cwd string
}

func NewGrepTool(cwd string) *GrepTool { return &GrepTool{cwd: cwd} }

func (t *GrepTool) Name() string                         { return "grep" }
func (t *GrepTool) IsReadOnly() bool                     { return true }
func (t *GrepTool) IsDestructive(_ json.RawMessage) bool { return false }

func (t *GrepTool) Description() string {
	return "Search file contents using a regular expression. Returns matching lines with file and line number."
}

func (t *GrepTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {
				"type": "string",
				"description": "Regular expression pattern to search for"
			},
			"path": {
				"type": "string",
				"description": "File or directory to search in (default: current directory)"
			},
			"glob": {
				"type": "string",
				"description": "Glob pattern to filter files (e.g. '*.go', '*.{ts,tsx}')"
			},
			"case_insensitive": {
				"type": "boolean",
				"description": "Case insensitive search (default false)"
			}
		},
		"required": ["pattern"]
	}`)
}

func (t *GrepTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		Glob            string `json:"glob"`
		CaseInsensitive bool   `json:"case_insensitive"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("grep: invalid input: %w", err)
	}
	if args.Pattern == "" {
		return "", fmt.Errorf("grep: pattern is required")
	}

	patternStr := args.Pattern
	if args.CaseInsensitive {
		patternStr = "(?i)" + patternStr
	}
	re, err := regexp.Compile(patternStr)
	if err != nil {
		return "", fmt.Errorf("grep: invalid pattern: %w", err)
	}

	searchPath := t.cwd
	if args.Path != "" {
		var err error
		searchPath, err = resolveScoped(t.cwd, args.Path)
		if err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}
	}

	var results []string
	err = filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if info.IsDir() {
			// Skip hidden and vendor dirs.
			base := filepath.Base(path)
			if base != "." && strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			if base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		// Glob filter.
		if args.Glob != "" {
			matched, _ := filepath.Match(args.Glob, filepath.Base(path))
			if !matched {
				return nil
			}
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if re.MatchString(line) {
				rel, _ := filepath.Rel(t.cwd, path)
				results = append(results, fmt.Sprintf("%s:%d: %s", rel, lineNum, line))
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("grep: walk: %w", err)
	}

	if len(results) == 0 {
		return "No matches found", nil
	}
	if len(results) > 500 {
		results = results[:500]
		results = append(results, "... (truncated at 500 matches)")
	}
	return strings.Join(results, "\n"), nil
}
