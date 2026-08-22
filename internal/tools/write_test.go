package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsBlockedPath(t *testing.T) {
	blocked := []string{
		".env", ".gitconfig", ".zshrc", ".bashrc",
		"secret.pem", "cert.key", "bundle.p12", "keystore.pfx",
		"/home/user/.env", "/etc/.gitconfig",
	}
	for _, p := range blocked {
		if !isBlockedPath(p) {
			t.Errorf("expected %q to be blocked", p)
		}
	}

	allowed := []string{
		"main.go", "README.md", "config.yaml",
		"/tmp/output.tf", "src/main.py",
	}
	for _, p := range allowed {
		if isBlockedPath(p) {
			t.Errorf("expected %q to be allowed", p)
		}
	}
}

func TestWriteTool_Execute(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteTool(dir)

	input, _ := json.Marshal(map[string]string{
		"file_path": "output.tf",
		"content":   "resource \"aws_s3_bucket\" \"b\" {}",
	})

	out, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}

	data, err := os.ReadFile(filepath.Join(dir, "output.tf"))
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if string(data) != `resource "aws_s3_bucket" "b" {}` {
		t.Errorf("file content mismatch: %q", string(data))
	}
}

func TestWriteTool_BlockedPath(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteTool(dir)

	input, _ := json.Marshal(map[string]string{
		"file_path": ".env",
		"content":   "SECRET=oops",
	})

	_, err := tool.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for blocked path")
	}
}

func TestWriteTool_AbsolutePathOutsideCwdRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "pwned.tf")

	tool := NewWriteTool(dir)
	input, _ := json.Marshal(map[string]string{
		"file_path": target,
		"content":   "resource \"evil\" \"x\" {}",
	})
	_, err := tool.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error writing outside the task working directory")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("file should not have been written outside cwd")
	}
}

func TestWriteTool_MissingFilePath(t *testing.T) {
	tool := NewWriteTool(t.TempDir())
	input, _ := json.Marshal(map[string]string{"content": "data"})
	_, err := tool.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for missing file_path")
	}
}

func TestWriteTool_CreatesSubdirectories(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteTool(dir)

	input, _ := json.Marshal(map[string]string{
		"file_path": "a/b/c/file.tf",
		"content":   "hello",
	})

	_, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a/b/c/file.tf")); err != nil {
		t.Errorf("expected file to exist: %v", err)
	}
}
