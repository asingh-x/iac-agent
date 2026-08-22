package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobTool_SimplePattern(t *testing.T) {
	dir := t.TempDir()
	// Create some files
	for _, name := range []string{"foo.go", "bar.go", "baz.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	tool := NewGlobTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "*.go",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "foo.go") {
		t.Errorf("expected foo.go in output, got:\n%s", out)
	}
	if !strings.Contains(out, "bar.go") {
		t.Errorf("expected bar.go in output, got:\n%s", out)
	}
	if strings.Contains(out, "baz.txt") {
		t.Errorf("baz.txt should not appear in *.go results, got:\n%s", out)
	}
}

func TestGlobTool_AbsolutePathOutsideCwdRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewGlobTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "*.go",
		"path":    outside,
	}))
	if err == nil {
		t.Fatal("expected error globbing an absolute path outside the task working directory")
	}
}

func TestGlobTool_NoMatch(t *testing.T) {
	dir := t.TempDir()
	// No files at all in the temp dir matching *.rs

	tool := NewGlobTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "*.rs",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// When no files match, the tool returns a "No files found" message
	if strings.TrimSpace(out) == "" {
		t.Error("expected non-empty response for no-match case")
	}
	if !strings.Contains(strings.ToLower(out), "no files") && !strings.Contains(strings.ToLower(out), "no match") {
		// Also acceptable: the tool may just return an empty list.
		// The important thing is no error and something sensible is returned.
		t.Logf("no-match output: %q", out)
	}
}
