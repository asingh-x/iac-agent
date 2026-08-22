package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditTool_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit_me.txt")
	original := "Hello, world!\nThis is a unique line.\nGoodbye.\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path":  path,
		"old_string": "unique line",
		"new_string": "replaced line",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "replaced") {
		t.Errorf("expected success message, got: %s", out)
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "unique line") {
		t.Error("old_string should be gone from file")
	}
	if !strings.Contains(string(data), "replaced line") {
		t.Error("new_string should be in file")
	}
}

func TestEditTool_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("some content here\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path":  path,
		"old_string": "this string does not exist",
		"new_string": "replacement",
	}))
	if err == nil {
		t.Fatal("expected error when old_string not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestEditTool_AbsolutePathOutsideCwdRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(target, []byte("original content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path":  target,
		"old_string": "original",
		"new_string": "tampered",
	}))
	if err == nil {
		t.Fatal("expected error editing a file outside the task working directory")
	}
	data, _ := os.ReadFile(target)
	if strings.Contains(string(data), "tampered") {
		t.Fatal("file outside cwd should not have been modified")
	}
}

func TestEditTool_Ambiguous(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	// old_string appears twice
	content := "duplicate\nsome other text\nduplicate\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path":  path,
		"old_string": "duplicate",
		"new_string": "single",
	}))
	if err == nil {
		t.Fatal("expected error when old_string is ambiguous")
	}
	// The error message mentions the count or replace_all
	if !strings.Contains(err.Error(), "2") && !strings.Contains(err.Error(), "replace_all") {
		t.Errorf("expected ambiguity error mentioning count or replace_all, got: %v", err)
	}
}
