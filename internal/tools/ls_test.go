package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLsTool_ListsEntries(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "alpha.txt", "content a")
	writeTemp(t, dir, "beta.go", "content b")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	tool := NewLsTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "alpha.txt") {
		t.Errorf("expected alpha.txt in output, got:\n%s", out)
	}
	if !strings.Contains(out, "beta.go") {
		t.Errorf("expected beta.go in output, got:\n%s", out)
	}
	// Directories should be rendered with a trailing slash.
	if !strings.Contains(out, "subdir/") {
		t.Errorf("expected subdir/ in output, got:\n%s", out)
	}
}

func TestLsTool_FileSizeInOutput(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "data.txt", "hello")

	tool := NewLsTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Output format: "filename (N bytes)"
	if !strings.Contains(out, "bytes") {
		t.Errorf("expected byte size in output, got:\n%s", out)
	}
}

func TestLsTool_NonExistentPath(t *testing.T) {
	dir := t.TempDir()
	tool := NewLsTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"path": "no_such_directory",
	}))
	if err == nil {
		t.Fatal("expected error for non-existent directory, got nil")
	}
}

func TestLsTool_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(empty, 0755); err != nil {
		t.Fatal(err)
	}

	tool := NewLsTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"path": "emptydir",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "empty") {
		t.Errorf("expected empty-directory message, got: %q", out)
	}
}

func TestLsTool_PathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	// Create a sibling directory with a known file inside it.
	sibling := t.TempDir()
	writeTemp(t, sibling, "private.txt", "private content")

	tool := NewLsTool(dir)
	// Attempt to list the sibling via a relative traversal.
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"path": "../" + filepath.Base(sibling),
	}))
	if err == nil {
		t.Fatal("expected error listing a directory outside the task working directory")
	}
}

func TestLsTool_AbsolutePathOutsideCwdRejected(t *testing.T) {
	dir := t.TempDir()
	sibling := t.TempDir()
	writeTemp(t, sibling, "private.txt", "private content")

	tool := NewLsTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"path": sibling,
	}))
	if err == nil {
		t.Fatal("expected error listing an absolute path outside the task working directory")
	}
}

func TestLsTool_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "inner")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTemp(t, subDir, "inner_file.go", "package inner")

	tool := NewLsTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"path": subDir, // absolute path
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "inner_file.go") {
		t.Errorf("expected inner_file.go in output, got:\n%s", out)
	}
}

func TestLsTool_RelativePath(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTemp(t, subDir, "rel_file.txt", "data")

	tool := NewLsTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"path": "sub",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "rel_file.txt") {
		t.Errorf("expected rel_file.txt in output, got:\n%s", out)
	}
}

func TestLsTool_DefaultsToWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "cwd_file.tf", "resource {}")

	tool := NewLsTool(dir)
	// No path argument — should list cwd.
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "cwd_file.tf") {
		t.Errorf("expected cwd_file.tf in output, got:\n%s", out)
	}
}

func TestLsTool_Metadata(t *testing.T) {
	tool := NewLsTool(t.TempDir())
	if tool.Name() != "ls" {
		t.Errorf("expected name 'ls', got %q", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}
	if len(tool.Schema()) == 0 {
		t.Error("Schema() is empty")
	}
	if !tool.IsReadOnly() {
		t.Error("ls should be read-only")
	}
	if tool.IsDestructive(nil) {
		t.Error("ls should not be destructive")
	}
}
