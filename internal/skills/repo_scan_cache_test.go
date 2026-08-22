package skills

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tf-agent/tf-agent/internal/db"
)

// initGitRepo creates a real git repo in dir with the given files committed,
// so gitIdentity() has a real commit sha to work with. Skips the test if git
// isn't available in the environment.
func initGitRepo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for name, content := range files {
		full := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(full), 0755)
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "initial")

	out, err := runGit(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return out
}

func TestRepoScan_CachesTerraformIndexAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	sha := initGitRepo(t, dir, map[string]string{
		"main.tf": `resource "aws_s3_bucket" "main" {}`,
	})

	store := db.NewMemoryStore()
	s := NewRepoScanSkill(store)
	input, _ := json.Marshal(map[string]any{"path": dir})

	out1, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if !strings.Contains(out1, "aws_s3_bucket") {
		t.Errorf("expected resource in first scan output, got:\n%s", out1)
	}
	if strings.Contains(out1, "cached index") {
		t.Errorf("first scan should be a fresh parse, not a cache hit:\n%s", out1)
	}

	// The cache entry must now exist for this repo+commit.
	entry, err := store.GetRepoIndex(context.Background(), dir, sha)
	if err != nil {
		t.Fatalf("GetRepoIndex: %v", err)
	}
	if entry == nil {
		t.Fatal("expected a repo index entry to be saved after the first scan")
	}

	out2, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if !strings.Contains(out2, "aws_s3_bucket") {
		t.Errorf("expected resource in second (cached) scan output, got:\n%s", out2)
	}
	if !strings.Contains(out2, "cached index") {
		t.Errorf("second scan should report a cache hit, got:\n%s", out2)
	}
}

func TestRepoScan_NonGitDir_NoCaching(t *testing.T) {
	dir := t.TempDir() // no .git here
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_vpc" "main" {}`), 0644)

	store := db.NewMemoryStore()
	s := NewRepoScanSkill(store)
	input, _ := json.Marshal(map[string]any{"path": dir})

	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "cached index") {
		t.Errorf("a non-git directory must never report a cache hit, got:\n%s", out)
	}
	if !strings.Contains(out, "aws_vpc") {
		t.Errorf("expected the resource to still be found via a fresh parse, got:\n%s", out)
	}
}

func TestRepoScan_NilStore_NoCaching(t *testing.T) {
	dir := t.TempDir()
	_ = initGitRepo(t, dir, map[string]string{
		"main.tf": `resource "aws_vpc" "main" {}`,
	})

	s := NewRepoScanSkill(nil) // e.g. an isolated sub-agent with no store wired
	input, _ := json.Marshal(map[string]any{"path": dir})

	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "cached index") {
		t.Errorf("a nil store must never report a cache hit, got:\n%s", out)
	}
	if !strings.Contains(out, "aws_vpc") {
		t.Errorf("expected the resource to still be found via a fresh parse, got:\n%s", out)
	}
}

func TestRepoScan_CommitChange_InvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	sha1 := initGitRepo(t, dir, map[string]string{
		"main.tf": `resource "aws_s3_bucket" "main" {}`,
	})

	store := db.NewMemoryStore()
	s := NewRepoScanSkill(store)
	input, _ := json.Marshal(map[string]any{"path": dir})

	if _, err := s.Execute(context.Background(), input); err != nil {
		t.Fatalf("first Execute: %v", err)
	}

	// Commit a new resource on top of the existing one.
	if err := os.WriteFile(filepath.Join(dir, "extra.tf"), []byte(`resource "aws_iam_role" "extra" {}`), 0644); err != nil {
		t.Fatalf("write extra.tf: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "second commit")

	sha2, err := runGit(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if sha1 == sha2 {
		t.Fatal("expected the commit sha to change after a new commit")
	}

	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if strings.Contains(out, "cached index") {
		t.Errorf("a new commit must not reuse the old commit's cache entry, got:\n%s", out)
	}
	if !strings.Contains(out, "aws_iam_role") {
		t.Errorf("expected the newly added resource to appear after re-parsing, got:\n%s", out)
	}
}
