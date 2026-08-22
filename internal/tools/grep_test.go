package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepTool_BasicMatch(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "a.go", "package main\n\nfunc Hello() string {\n\treturn \"hello world\"\n}\n")
	writeTemp(t, dir, "b.go", "package main\n\nfunc Goodbye() string {\n\treturn \"goodbye\"\n}\n")

	tool := NewGrepTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "Hello",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Hello") {
		t.Errorf("expected 'Hello' in output, got:\n%s", out)
	}
}

func TestGrepTool_NoMatch(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "file.txt", "line one\nline two\nline three\n")

	tool := NewGrepTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "zzznomatchzzz",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "no match") {
		t.Errorf("expected no-match message, got: %q", out)
	}
}

func TestGrepTool_InvalidRegex(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "file.txt", "some content\n")

	tool := NewGrepTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "[invalid(regex",
	}))
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
	if !strings.Contains(err.Error(), "invalid pattern") {
		t.Errorf("expected 'invalid pattern' in error, got: %v", err)
	}
}

func TestGrepTool_GlobFilter(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "main.go", "package main\nfunc main() {}\n")
	writeTemp(t, dir, "README.md", "func notGoCode() {}\n")

	tool := NewGrepTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "func",
		"glob":    "*.go",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("expected main.go in output, got:\n%s", out)
	}
	if strings.Contains(out, "README.md") {
		t.Errorf("README.md should be excluded by glob filter, got:\n%s", out)
	}
}

func TestGrepTool_RelativePathOutsideCwd(t *testing.T) {
	dir := t.TempDir()

	// Write a file in a sibling temp directory.
	outsideDir := t.TempDir()
	writeTemp(t, outsideDir, "external.txt", "external content\n")

	tool := NewGrepTool(dir)
	// A relative path that resolves outside cwd must be rejected.
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "external content",
		"path":    "../" + filepath.Base(outsideDir),
	}))
	if err == nil {
		t.Fatal("expected error searching a path outside the task working directory")
	}
}

func TestGrepTool_AbsolutePathOutsideCwdRejected(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	writeTemp(t, outsideDir, "external.txt", "external content\n")

	tool := NewGrepTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "external content",
		"path":    outsideDir,
	}))
	if err == nil {
		t.Fatal("expected error searching an absolute path outside the task working directory")
	}
}

func TestGrepTool_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "notes.txt", "The Quick Brown Fox\n")

	tool := NewGrepTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern":          "quick brown",
		"case_insensitive": true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(strings.ToLower(out), "no match") {
		t.Errorf("expected case-insensitive match, got no-match: %q", out)
	}
	if !strings.Contains(out, "Quick Brown") {
		t.Errorf("expected matched line in output, got:\n%s", out)
	}
}

func TestGrepTool_MultipleMatchesInFile(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "dup.txt", "foo bar\nbaz\nfoo qux\n")

	tool := NewGrepTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "foo",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should find two lines containing "foo".
	count := strings.Count(out, "foo")
	if count < 2 {
		t.Errorf("expected at least 2 matches for 'foo', got %d in:\n%s", count, out)
	}
}

func TestGrepTool_LineNumbers(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "lines.txt", "alpha\nbeta\ngamma\n")

	tool := NewGrepTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "beta",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Output format: "relpath:linenum: content"
	if !strings.Contains(out, ":2:") {
		t.Errorf("expected line number 2 in output, got:\n%s", out)
	}
}

func TestGrepTool_SkipsHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	// Create a hidden directory with a matching file inside it.
	hiddenDir := filepath.Join(dir, ".hidden")
	if err := os.Mkdir(hiddenDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTemp(t, hiddenDir, "secret.txt", "findme\n")
	// Also create a visible file with the same content.
	writeTemp(t, dir, "visible.txt", "findme\n")

	tool := NewGrepTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "findme",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, ".hidden") {
		t.Errorf("expected hidden directory to be skipped, got:\n%s", out)
	}
	if !strings.Contains(out, "visible.txt") {
		t.Errorf("expected visible.txt in results, got:\n%s", out)
	}
}

func TestGrepTool_EmptyPattern(t *testing.T) {
	dir := t.TempDir()
	tool := NewGrepTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"pattern": "",
	}))
	if err == nil {
		t.Fatal("expected error for empty pattern, got nil")
	}
}

func TestGrepTool_Metadata(t *testing.T) {
	tool := NewGrepTool(t.TempDir())
	if tool.Name() != "grep" {
		t.Errorf("expected name 'grep', got %q", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}
	if len(tool.Schema()) == 0 {
		t.Error("Schema() is empty")
	}
	if !tool.IsReadOnly() {
		t.Error("grep should be read-only")
	}
	if tool.IsDestructive(nil) {
		t.Error("grep should not be destructive")
	}
}
