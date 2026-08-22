package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("writeTemp: %v", err)
	}
	return p
}

func TestReadTool_Basic(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "file.txt", "line1\nline2\nline3\n")

	tool := NewReadTool(dir)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect cat -n style: "1\tline1\n" etc.
	for i, want := range []string{"1\tline1", "2\tline2", "3\tline3"} {
		if !strings.Contains(out, want) {
			t.Errorf("line %d: expected %q in output:\n%s", i+1, want, out)
		}
	}
}

func TestReadTool_OffsetLimit(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	content := strings.Join(lines, "\n") + "\n"
	path := writeTemp(t, dir, "file.txt", content)

	tool := NewReadTool(dir)
	// offset=3 means start at line 3; limit=2 means return 2 lines (lines 3 and 4)
	out, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"offset":    3,
		"limit":     2,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "3\tline3") {
		t.Errorf("expected line3 in output, got:\n%s", out)
	}
	if !strings.Contains(out, "4\tline4") {
		t.Errorf("expected line4 in output, got:\n%s", out)
	}
	if strings.Contains(out, "line5") {
		t.Errorf("line5 should NOT be present (limit=2), got:\n%s", out)
	}
}

func TestReadTool_AbsolutePathOutsideCwdRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir() // a different temp dir, not under dir
	secret := writeTemp(t, outside, "secret.txt", "top secret")

	tool := NewReadTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": secret,
	}))
	if err == nil {
		t.Fatal("expected error reading a file outside the task working directory")
	}
}

func TestReadTool_NonExistent(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadTool(dir)
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": filepath.Join(dir, "no_such_file.txt"),
	}))
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}
