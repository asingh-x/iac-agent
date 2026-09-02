package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tf-agent/tf-agent/internal/sandbox"
	"github.com/tf-agent/tf-agent/internal/taskctx"
)

// --- Registry ---

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	r.Register(&ClarifierSkill{})

	s, ok := r.Get("clarifier")
	if !ok {
		t.Fatal("expected to find clarifier skill")
	}
	if s.Name() != "clarifier" {
		t.Errorf("Name = %q, want clarifier", s.Name())
	}
}

func TestRegistry_Get_NotFound(t *testing.T) {
	r := NewRegistry()
	_, ok := r.Get("nonexistent")
	if ok {
		t.Error("expected ok=false for unknown skill")
	}
}

func TestRegistry_Names(t *testing.T) {
	r := NewRegistry()
	r.Register(&ClarifierSkill{})
	r.Register(&RepoScanSkill{})

	names := r.Names()
	if len(names) != 2 {
		t.Errorf("expected 2 names, got %d", len(names))
	}
}

func TestRegistry_Schemas(t *testing.T) {
	r := NewRegistry()
	r.Register(&ClarifierSkill{})
	r.Register(&ValidateSkill{})

	schemas := r.Schemas()
	if len(schemas) != 2 {
		t.Errorf("expected 2 schemas, got %d", len(schemas))
	}
	for _, s := range schemas {
		if s.Name == "" {
			t.Error("schema name should not be empty")
		}
		if s.Description == "" {
			t.Error("schema description should not be empty")
		}
	}
}

func TestRegistry_AllPrompts(t *testing.T) {
	r := NewRegistry()
	r.Register(&ClarifierSkill{})
	r.Register(&GenerateSkill{})

	prompts := r.AllPrompts()
	if len(prompts) == 0 {
		t.Error("expected non-empty prompts map")
	}
	for name, p := range prompts {
		if p == "" {
			t.Errorf("skill %q returned empty prompt", name)
		}
	}
}

func TestRegistry_Execute_Unknown(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "unknown_skill", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for unknown skill")
	}
}

func TestRegistry_Execute_Known(t *testing.T) {
	r := NewRegistry()
	r.Register(&ClarifierSkill{})

	input, _ := json.Marshal(map[string]any{
		"request":   "create terraform",
		"questions": []string{"Which cloud provider?"},
	})
	out, err := r.Execute(context.Background(), "clarifier", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Error("expected non-empty output")
	}
}

// --- ClarifierSkill ---

func TestClarifier_Execute_WithQuestions(t *testing.T) {
	s := &ClarifierSkill{}
	input, _ := json.Marshal(map[string]any{
		"request":   "create terraform for infra",
		"questions": []string{"Which cloud provider?", "Which region?", "What environment?"},
	})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Which cloud provider?") {
		t.Errorf("output missing first question: %q", out)
	}
	if !strings.Contains(out, "create terraform for infra") {
		t.Errorf("output missing request context: %q", out)
	}
}

func TestClarifier_Execute_MaxThreeQuestions(t *testing.T) {
	s := &ClarifierSkill{}
	input, _ := json.Marshal(map[string]any{
		"request":   "test",
		"questions": []string{"Q1", "Q2", "Q3", "Q4", "Q5"},
	})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "Q4") {
		t.Error("output should not include more than 3 questions")
	}
}

func TestClarifier_Execute_NoQuestions(t *testing.T) {
	s := &ClarifierSkill{}
	input, _ := json.Marshal(map[string]any{
		"request":   "test",
		"questions": []string{},
	})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Error("expected non-empty output even with no questions")
	}
}

func TestClarifier_Execute_InvalidInput(t *testing.T) {
	s := &ClarifierSkill{}
	_, err := s.Execute(context.Background(), json.RawMessage(`not-valid-json`))
	if err == nil {
		t.Error("expected error for invalid JSON input")
	}
}

func TestClarifier_Metadata(t *testing.T) {
	s := &ClarifierSkill{}
	if s.Name() != "clarifier" {
		t.Errorf("Name = %q", s.Name())
	}
	if !s.IsReadOnly() {
		t.Error("clarifier should be read-only")
	}
	if s.IsDestructive(nil) {
		t.Error("clarifier should not be destructive")
	}
	if s.Prompt() == "" {
		t.Error("Prompt should be non-empty")
	}
	if s.Schema() == nil {
		t.Error("Schema should not be nil")
	}
}

// --- GenerateSkill ---

func TestGenerate_Execute_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewGenerateSkill(dir)

	input, _ := json.Marshal(map[string]any{
		"files": map[string]string{
			"main.tf":      `resource "aws_s3_bucket" "main" {}`,
			"variables.tf": `variable "env" { type = string }`,
		},
	})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "main.tf") {
		t.Errorf("output missing main.tf: %q", out)
	}

	// Verify files were actually written.
	content, err := os.ReadFile(filepath.Join(dir, "main.tf"))
	if err != nil {
		t.Fatalf("main.tf not written: %v", err)
	}
	if !strings.Contains(string(content), "aws_s3_bucket") {
		t.Errorf("main.tf content wrong: %q", string(content))
	}
}

func TestGenerate_Execute_CustomOutputDir(t *testing.T) {
	baseDir := t.TempDir()
	subDir := filepath.Join(baseDir, "terraform", "s3")

	s := NewGenerateSkill("")
	input, _ := json.Marshal(map[string]any{
		"files":      map[string]string{"outputs.tf": `output "bucket_arn" {}`},
		"output_dir": subDir,
	})
	_, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subDir, "outputs.tf")); err != nil {
		t.Errorf("outputs.tf not created in custom dir: %v", err)
	}
}

func TestGenerate_Execute_EmptyFiles(t *testing.T) {
	s := NewGenerateSkill(t.TempDir())
	input, _ := json.Marshal(map[string]any{"files": map[string]string{}})
	_, err := s.Execute(context.Background(), input)
	if err == nil {
		t.Error("expected error for empty files map")
	}
}

func TestGenerate_Execute_InvalidInput(t *testing.T) {
	s := NewGenerateSkill(t.TempDir())
	_, err := s.Execute(context.Background(), json.RawMessage(`bad json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestGenerate_Metadata(t *testing.T) {
	s := NewGenerateSkill("")
	if s.Name() != "generate_terraform" {
		t.Errorf("Name = %q", s.Name())
	}
	if s.IsReadOnly() {
		t.Error("generate should not be read-only")
	}
	if s.Prompt() == "" {
		t.Error("Prompt should be non-empty")
	}
}

// --- RepoScanSkill ---

func TestRepoScan_Execute_ReturnsStructure(t *testing.T) {
	dir := t.TempDir()

	// Create some files to scan.
	_ = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_vpc" "main" {}`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "variables.tf"), []byte(`variable "region" {}`), 0644)
	subDir := filepath.Join(dir, "modules")
	_ = os.MkdirAll(subDir, 0755)
	_ = os.WriteFile(filepath.Join(subDir, "eks.tf"), []byte(`module "eks" {}`), 0644)

	s := &RepoScanSkill{}
	input, _ := json.Marshal(map[string]any{"path": dir})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "main.tf") {
		t.Errorf("output missing main.tf: %q", out)
	}
	if !strings.Contains(out, "variables.tf") {
		t.Errorf("output missing variables.tf: %q", out)
	}
	if !strings.Contains(out, "modules") {
		t.Errorf("output missing modules dir: %q", out)
	}
}

func TestRepoScan_Execute_DefaultsToCurrentDir(t *testing.T) {
	s := &RepoScanSkill{}
	// Empty input — defaults to CWD.
	out, err := s.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Error("expected non-empty output")
	}
}

func TestRepoScan_Execute_MaxDepth(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b", "c", "d")
	_ = os.MkdirAll(deep, 0755)
	_ = os.WriteFile(filepath.Join(deep, "deep.tf"), []byte(""), 0644)

	s := &RepoScanSkill{}
	input, _ := json.Marshal(map[string]any{"path": dir, "max_depth": 1})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "deep.tf") {
		t.Error("max_depth=1 should not include files 4 levels deep")
	}
}

func TestRepoScan_Metadata(t *testing.T) {
	s := &RepoScanSkill{}
	if s.Name() != "repo_scan" {
		t.Errorf("Name = %q", s.Name())
	}
	if !s.IsReadOnly() {
		t.Error("repo_scan should be read-only")
	}
	if s.Prompt() == "" {
		t.Error("Prompt should be non-empty")
	}
}

// --- SecurityScanSkill (parsing only — checkov not required) ---

func TestParseCheckovOutput_PassFail(t *testing.T) {
	data := []byte(`{
		"results": {
			"passed_checks": [{"check_id":"CKV_AWS_1","resource":"aws_s3_bucket.main"}],
			"failed_checks": [
				{
					"check_id":"CKV_AWS_2",
					"resource":"aws_s3_bucket.main",
					"check":{"name":"Ensure S3 bucket has versioning"},
					"repo_file_path":"main.tf",
					"file_line_range":[10,15]
				}
			]
		},
		"summary": {"passed":1,"failed":1}
	}`)
	out, err := parseCheckovOutput(data)
	if err != nil {
		t.Fatalf("parseCheckovOutput: %v", err)
	}
	if !strings.Contains(out, "1 passed") {
		t.Errorf("output missing pass count: %q", out)
	}
	if !strings.Contains(out, "1 failed") {
		t.Errorf("output missing fail count: %q", out)
	}
	if !strings.Contains(out, "CKV_AWS_2") {
		t.Errorf("output missing failed check ID: %q", out)
	}
	if !strings.Contains(out, "versioning") {
		t.Errorf("output missing check name: %q", out)
	}
}

func TestParseCheckovOutput_ArrayFormat(t *testing.T) {
	// Checkov sometimes returns a JSON array.
	data := []byte(`[{"results":{"passed_checks":[],"failed_checks":[]},"summary":{"passed":5,"failed":0}}]`)
	out, err := parseCheckovOutput(data)
	if err != nil {
		t.Fatalf("parseCheckovOutput: %v", err)
	}
	if !strings.Contains(out, "5 passed") {
		t.Errorf("expected 5 passed, got: %q", out)
	}
}

func TestParseCheckovOutput_Empty(t *testing.T) {
	out, err := parseCheckovOutput([]byte{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == "" {
		t.Error("expected non-empty output for empty input")
	}
}

func TestSecurityScan_Metadata(t *testing.T) {
	s := &SecurityScanSkill{}
	if s.Name() != "SecurityScan" {
		t.Errorf("Name = %q", s.Name())
	}
	if !s.IsReadOnly() {
		t.Error("SecurityScan should be read-only")
	}
	if s.Prompt() == "" {
		t.Error("Prompt should be non-empty")
	}
}

// --- CreatePRSkill (metadata + parseRepoURL) ---

func TestParseRepoURL_Valid(t *testing.T) {
	cases := []struct {
		input     string
		wantOwner string
		wantRepo  string
	}{
		{"github.com/org/repo", "org", "repo"},
		{"https://github.com/org/repo", "org", "repo"},
		{"http://github.com/org/repo", "org", "repo"},
	}
	for _, c := range cases {
		owner, repo, err := parseRepoURL(c.input)
		if err != nil {
			t.Errorf("parseRepoURL(%q): unexpected error: %v", c.input, err)
			continue
		}
		if owner != c.wantOwner {
			t.Errorf("parseRepoURL(%q): owner = %q, want %q", c.input, owner, c.wantOwner)
		}
		if repo != c.wantRepo {
			t.Errorf("parseRepoURL(%q): repo = %q, want %q", c.input, repo, c.wantRepo)
		}
	}
}

func TestParseRepoURL_Invalid(t *testing.T) {
	cases := []string{"", "github.com/onlyone", "not-a-url"}
	for _, c := range cases {
		_, _, err := parseRepoURL(c)
		if err == nil {
			t.Errorf("parseRepoURL(%q): expected error, got nil", c)
		}
	}
}

func TestCreatePR_Metadata(t *testing.T) {
	s := &CreatePRSkill{}
	if s.Name() != "CreatePR" {
		t.Errorf("Name = %q", s.Name())
	}
	if s.IsReadOnly() {
		t.Error("CreatePR should not be read-only")
	}
	if s.Prompt() == "" {
		t.Error("Prompt should be non-empty")
	}
}

func TestCreatePR_MissingToken(t *testing.T) {
	s := &CreatePRSkill{}
	// No GITHUB_TOKEN in env, no context creds.
	input, _ := json.Marshal(map[string]any{
		"repo_url": "github.com/org/repo",
		"branch":   "tf-agent/test",
		"title":    "test PR",
		"body":     "test",
		"files":    map[string]string{"main.tf": ""},
	})
	_, err := s.Execute(context.Background(), input)
	if err == nil {
		t.Error("expected error when GITHUB_TOKEN is not set")
	}
	if !strings.Contains(err.Error(), "github_token") {
		t.Errorf("error should mention github_token: %v", err)
	}
}

func TestCreatePR_DeterministicBranch_UsesTaskID(t *testing.T) {
	var capturedRef string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/main"):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "base-sha"}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
			w.WriteHeader(http.StatusNotFound) // branch doesn't exist yet
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			capturedRef = body["ref"]
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusNotFound) // file doesn't exist yet
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]any{}) // no existing PR
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/org/repo/pull/1"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := &CreatePRSkill{baseURL: srv.URL}
	ctx := taskctx.WithCredentials(context.Background(), taskctx.Credentials{
		GitHubToken: "tok",
		TaskID:      "task-abc-123",
	})
	input, _ := json.Marshal(map[string]any{
		"repo_url": "github.com/org/repo",
		"branch":   "whatever-the-llm-suggested",
		"title":    "test PR",
		"body":     "test",
		"files":    map[string]string{"main.tf": "content"},
	})
	_, err := s.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if capturedRef != "refs/heads/iac-agent/task-task-abc-123" {
		t.Errorf("branch ref = %q, want deterministic task-based ref, not the LLM-suggested name", capturedRef)
	}
}

func TestCreatePR_Idempotent_ExistingBranchAndPR_ReturnsExistingURL(t *testing.T) {
	var createRefCalled, createPRCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// Deliberately no handler for GET /git/ref/heads/main: on the
		// fully-idempotent path (branch+PR both already exist) the base SHA
		// is never needed, so this test asserts that call is skipped
		// entirely — hitting it falls through to the "unexpected request"
		// default below and fails the test.
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/iac-agent"):
			w.WriteHeader(http.StatusOK) // branch already exists
			json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "branch-sha"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			createRefCalled = true
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]map[string]string{{"html_url": "https://github.com/org/repo/pull/42"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			createPRCalled = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/org/repo/pull/999"})
		default:
			t.Errorf("unexpected request on a fully-idempotent retry: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := &CreatePRSkill{baseURL: srv.URL}
	ctx := taskctx.WithCredentials(context.Background(), taskctx.Credentials{GitHubToken: "tok", TaskID: "task-abc-123"})
	input, _ := json.Marshal(map[string]any{
		"repo_url": "github.com/org/repo", "branch": "ignored", "title": "t", "body": "b",
		"files": map[string]string{"main.tf": "content"},
	})
	out, err := s.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if createRefCalled {
		t.Error("createRef should not be called when the branch already exists")
	}
	if createPRCalled {
		t.Error("createPR should not be called when a PR already exists for this branch")
	}
	if !strings.Contains(out, "https://github.com/org/repo/pull/42") {
		t.Errorf("expected the EXISTING PR URL in output, got: %q", out)
	}
}

func TestCreatePR_Idempotent_BranchExistsNoPR_ResumesFromPRCreation(t *testing.T) {
	var createRefCalled, filePutCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/main"):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "base-sha"}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/iac-agent"):
			w.WriteHeader(http.StatusOK) // branch already exists (prior attempt got this far)
			json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "branch-sha"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			createRefCalled = true
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]any{}) // no PR yet — prior attempt crashed before opening it
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			filePutCalled = true
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/org/repo/pull/999"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := &CreatePRSkill{baseURL: srv.URL}
	ctx := taskctx.WithCredentials(context.Background(), taskctx.Credentials{GitHubToken: "tok", TaskID: "task-abc-123"})
	input, _ := json.Marshal(map[string]any{
		"repo_url": "github.com/org/repo", "branch": "ignored", "title": "t", "body": "b",
		"files": map[string]string{"main.tf": "content"},
	})
	out, err := s.Execute(ctx, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if createRefCalled {
		t.Error("createRef should not be called when the branch already exists")
	}
	if !filePutCalled {
		t.Error("files should be re-uploaded when resuming from the branch-exists-no-PR state")
	}
	if !strings.Contains(out, "https://github.com/org/repo/pull/999") {
		t.Errorf("expected the newly-created PR URL, got: %q", out)
	}
}

// --- ValidateSkill (metadata only — tflint/terraform not required) ---

func TestValidate_Metadata(t *testing.T) {
	s := &ValidateSkill{}
	if s.Name() != "validate_terraform" {
		t.Errorf("Name = %q", s.Name())
	}
	if !s.IsReadOnly() {
		t.Error("validate should be read-only")
	}
	if s.Prompt() == "" {
		t.Error("Prompt should be non-empty")
	}
}

func TestValidate_Execute_InvalidInput(t *testing.T) {
	s := &ValidateSkill{}
	_, err := s.Execute(context.Background(), json.RawMessage(`not json`))
	if err == nil {
		t.Error("expected error for invalid input")
	}
}

func TestValidate_Execute_MissingPath(t *testing.T) {
	// Simulate the sandboxed tools not being found (e.g. a stripped-down
	// sandbox image) via a fakeExecutor that errors on every call, rather
	// than the old nil-executor host fallback — sandboxed execution is now
	// mandatory, so a nil executor is a hard error tested separately (see
	// TestValidateSkill_NilExecutor_NoLongerFallsBackToHost).
	fe := &fakeExecutor{err: errors.New("exec: \"tflint\": executable file not found in $PATH")}
	s := NewValidateSkill(fe)
	input, _ := json.Marshal(map[string]any{"path": t.TempDir()})
	out, err := s.Execute(context.Background(), input)
	// Should not return a hard error — tools just won't be available.
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if out == "" {
		t.Error("expected non-empty output")
	}
}

// --- ValidateSkill structured error parsing ---
//
// The JSON fixtures below are real captured output, not hand-written: they
// come from actually running `terraform validate -json -no-color`
// (Terraform v1.11.4) and `tflint --format=json` (TFLint v0.61.0) against
// deliberately broken .tf fixtures, per the brief's instruction not to
// assume the tools' JSON shapes.
//
// Fixture 1 — main.tf with an unsupported argument and a reference to an
// undeclared variable:
//
//	resource "aws_instance" "example" {
//	  ami           = "ami-123456"
//	  instance_type = "t2.micro"
//	  foo           = "bar"
//	}
//
//	resource "aws_s3_bucket" "b" {
//	  bucket = var.undeclared_var
//	}
//
// Fixture 2 — main.tf with an unterminated quoted string (a syntax error
// tflint can't even parse past, landing in tflint's "errors" array rather
// than "issues"):
//
//	resource "aws_instance" "example" {
//	  ami = "ami-123456
//	}

const realTerraformValidateJSON = `{
  "format_version": "1.0",
  "valid": false,
  "error_count": 2,
  "warning_count": 0,
  "diagnostics": [
    {
      "severity": "error",
      "summary": "Unsupported argument",
      "detail": "An argument named \"foo\" is not expected here.",
      "range": {
        "filename": "main.tf",
        "start": {
          "line": 4,
          "column": 3,
          "byte": 98
        },
        "end": {
          "line": 4,
          "column": 6,
          "byte": 101
        }
      },
      "snippet": {
        "context": "resource \"aws_instance\" \"example\"",
        "code": "  foo           = \"bar\"",
        "start_line": 4,
        "highlight_start_offset": 2,
        "highlight_end_offset": 5,
        "values": []
      }
    },
    {
      "severity": "error",
      "summary": "Reference to undeclared input variable",
      "detail": "An input variable with the name \"undeclared_var\" has not been declared. This variable can be declared with a variable \"undeclared_var\" {} block.",
      "range": {
        "filename": "main.tf",
        "start": {
          "line": 8,
          "column": 12,
          "byte": 165
        },
        "end": {
          "line": 8,
          "column": 30,
          "byte": 183
        }
      },
      "snippet": {
        "context": "resource \"aws_s3_bucket\" \"b\"",
        "code": "  bucket = var.undeclared_var",
        "start_line": 8,
        "highlight_start_offset": 11,
        "highlight_end_offset": 29,
        "values": []
      }
    }
  ]
}`

const realTerraformValidateOKJSON = `{
  "format_version": "1.0",
  "valid": true,
  "error_count": 0,
  "warning_count": 0,
  "diagnostics": []
}`

const realTflintIssuesJSON = `{"issues":[{"rule":{"name":"terraform_required_version","severity":"warning","link":"https://github.com/terraform-linters/tflint-ruleset-terraform/blob/v0.14.1/docs/rules/terraform_required_version.md"},"message":"terraform \"required_version\" attribute is required","range":{"filename":"main.tf","start":{"line":1,"column":1},"end":{"line":1,"column":1}},"callers":[],"fixable":false,"fixed":false},{"rule":{"name":"terraform_required_providers","severity":"warning","link":"https://github.com/terraform-linters/tflint-ruleset-terraform/blob/v0.14.1/docs/rules/terraform_required_providers.md"},"message":"Missing version constraint for provider \"aws\" in ` + "`required_providers`" + `","range":{"filename":"main.tf","start":{"line":7,"column":1},"end":{"line":7,"column":29}},"callers":[],"fixable":false,"fixed":false}],"errors":[]}`

const realTflintErrorsJSON = `{"issues":[],"errors":[{"summary":"Invalid multi-line string","message":"Quoted strings may not be split over multiple lines. To produce a multi-line string, either use the \n escape to represent a newline character or use the \"heredoc\" multi-line template syntax.","severity":"error","range":{"filename":"main.tf","start":{"line":2,"column":20},"end":{"line":3,"column":1}}},{"summary":"Unterminated template string","message":"No closing marker was found for the string.","severity":"error","range":{"filename":"main.tf","start":{"line":2,"column":20},"end":{"line":3,"column":1}}}]}`

func TestParseTerraformValidateJSON_RealBrokenOutput(t *testing.T) {
	out, err := parseTerraformValidateJSON([]byte(realTerraformValidateJSON))
	if err != nil {
		t.Fatalf("parseTerraformValidateJSON: %v", err)
	}
	if !strings.Contains(out, "FAILED") {
		t.Errorf("expected FAILED status, got: %q", out)
	}
	if !strings.Contains(out, "2 error(s)") {
		t.Errorf("expected error count, got: %q", out)
	}
	if !strings.Contains(out, "main.tf:4") {
		t.Errorf("expected file:line for first diagnostic, got: %q", out)
	}
	if !strings.Contains(out, "Unsupported argument") {
		t.Errorf("expected first diagnostic summary, got: %q", out)
	}
	if !strings.Contains(out, `An argument named "foo" is not expected here.`) {
		t.Errorf("expected first diagnostic detail, got: %q", out)
	}
	if !strings.Contains(out, "main.tf:8") {
		t.Errorf("expected file:line for second diagnostic, got: %q", out)
	}
	if !strings.Contains(out, "Reference to undeclared input variable") {
		t.Errorf("expected second diagnostic summary, got: %q", out)
	}
	// Must not just be the raw JSON reformatted — the noisy "snippet" field
	// (source context/highlight offsets) is not needed for a compact
	// actionable summary and should not leak through.
	if strings.Contains(out, "highlight_start_offset") || strings.Contains(out, "format_version") {
		t.Errorf("expected structured summary, not raw JSON fields: %q", out)
	}
}

func TestParseTerraformValidateJSON_RealValidOutput(t *testing.T) {
	out, err := parseTerraformValidateJSON([]byte(realTerraformValidateOKJSON))
	if err != nil {
		t.Fatalf("parseTerraformValidateJSON: %v", err)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected OK status for valid config, got: %q", out)
	}
}

func TestParseTerraformValidateJSON_InvalidInput(t *testing.T) {
	cases := []string{"", "not json at all", "Usage: terraform validate [options] [dir]\n"}
	for _, c := range cases {
		if _, err := parseTerraformValidateJSON([]byte(c)); err == nil {
			t.Errorf("parseTerraformValidateJSON(%q): expected error, got nil", c)
		}
	}
}

func TestParseTflintJSON_RealIssuesOutput(t *testing.T) {
	out, err := parseTflintJSON([]byte(realTflintIssuesJSON))
	if err != nil {
		t.Fatalf("parseTflintJSON: %v", err)
	}
	if !strings.Contains(out, "2 issue(s), 0 error(s)") {
		t.Errorf("expected issue/error counts, got: %q", out)
	}
	if !strings.Contains(out, "main.tf:1") {
		t.Errorf("expected file:line for first issue, got: %q", out)
	}
	if !strings.Contains(out, "terraform_required_version") {
		t.Errorf("expected rule name, got: %q", out)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("expected severity, got: %q", out)
	}
}

func TestParseTflintJSON_RealErrorsOutput(t *testing.T) {
	out, err := parseTflintJSON([]byte(realTflintErrorsJSON))
	if err != nil {
		t.Fatalf("parseTflintJSON: %v", err)
	}
	if !strings.Contains(out, "0 issue(s), 2 error(s)") {
		t.Errorf("expected issue/error counts, got: %q", out)
	}
	if !strings.Contains(out, "main.tf:2") {
		t.Errorf("expected file:line, got: %q", out)
	}
	if !strings.Contains(out, "Invalid multi-line string") {
		t.Errorf("expected error summary, got: %q", out)
	}
}

func TestParseTflintJSON_InvalidInput(t *testing.T) {
	cases := []string{"", "not json", "tflint: command not found\n"}
	for _, c := range cases {
		if _, err := parseTflintJSON([]byte(c)); err == nil {
			t.Errorf("parseTflintJSON(%q): expected error, got nil", c)
		}
	}
}

// TestFormatTerraformValidateResult_FallsBackToRawText proves the
// fallback-to-raw-text path is actually exercised: when the command's
// combined output isn't valid terraform validate JSON (e.g. the binary
// isn't installed and exec.LookPath failed before anything ran), the
// original raw-text-plus-error behavior is preserved rather than a parse
// error being silently swallowed.
func TestFormatTerraformValidateResult_FallsBackToRawText(t *testing.T) {
	runErr := &exec.Error{Name: "terraform", Err: exec.ErrNotFound}
	got := formatTerraformValidateResult("", runErr)
	if !strings.Contains(got, "terraform not found or error") {
		t.Errorf("expected fallback error message, got: %q", got)
	}
	if !strings.Contains(got, runErr.Error()) {
		t.Errorf("expected underlying error text preserved, got: %q", got)
	}
}

func TestFormatTerraformValidateResult_PlainTextOutput_NoError(t *testing.T) {
	// Some failure modes (e.g. a crash) could plausibly print plain text to
	// stdout/stderr with a nil Go error. The raw text must still come
	// through rather than being dropped.
	got := formatTerraformValidateResult("panic: something went very wrong\n", nil)
	if !strings.Contains(got, "panic: something went very wrong") {
		t.Errorf("expected raw text passthrough, got: %q", got)
	}
}

// TestFormatTerraformValidateResult_NonZeroExitWithValidJSON_UsesStructuredSummary
// is the regression test for the pre-existing bug this change fixes:
// terraform validate exits 1 whenever it finds errors, so the old code
// treated that as "terraform not found or error" and discarded the actual
// (valid, informative) JSON diagnostics. A non-nil runErr must not override
// a successful JSON parse.
func TestFormatTerraformValidateResult_NonZeroExitWithValidJSON_UsesStructuredSummary(t *testing.T) {
	runErr := &exec.ExitError{}
	got := formatTerraformValidateResult(realTerraformValidateJSON, runErr)
	if strings.Contains(got, "not found or error") {
		t.Errorf("valid JSON diagnostics must not be discarded just because the process exited non-zero: %q", got)
	}
	if !strings.Contains(got, "Unsupported argument") {
		t.Errorf("expected structured diagnostics, got: %q", got)
	}
}

func TestFormatTflintResult_FallsBackToRawText(t *testing.T) {
	runErr := &exec.Error{Name: "tflint", Err: exec.ErrNotFound}
	got := formatTflintResult("", runErr)
	if !strings.Contains(got, "tflint not found or error") {
		t.Errorf("expected fallback error message, got: %q", got)
	}
	if !strings.Contains(got, runErr.Error()) {
		t.Errorf("expected underlying error text preserved, got: %q", got)
	}
}

func TestFormatTflintResult_NonZeroExitWithValidJSON_UsesStructuredSummary(t *testing.T) {
	// tflint exits 2 when it finds lint issues — same class of bug as
	// terraform validate's exit 1 above.
	runErr := &exec.ExitError{}
	got := formatTflintResult(realTflintIssuesJSON, runErr)
	if strings.Contains(got, "not found or error") {
		t.Errorf("valid JSON issues must not be discarded just because the process exited non-zero: %q", got)
	}
	if !strings.Contains(got, "terraform_required_version") {
		t.Errorf("expected structured issues, got: %q", got)
	}
}

// requireTerraformAndTflint skips the test if either binary isn't on PATH,
// so `go test ./...` stays green in environments without them installed.
func requireTerraformAndTflint(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not installed — skipping real-tool integration test")
	}
	if _, err := exec.LookPath("tflint"); err != nil {
		t.Skip("tflint not installed — skipping real-tool integration test")
	}
}

// hostExecPassthrough is a sandbox.Executor test double that actually runs
// the requested command on the host via os/exec, splitting stdout/stderr
// exactly like sandbox.DockerExecutor.Run does. ValidateSkill/SecurityScanSkill
// no longer shell out directly (sandboxed execution is mandatory — a nil
// executor is a hard error, see TestValidateSkill_NilExecutor_NoLongerFallsBackToHost),
// so real-tool integration tests route through this instead of relying on
// the removed nil-executor host fallback. It only stands in for a "sandbox"
// in the sense of implementing the Executor interface — it does not isolate
// anything, so it's only appropriate for tests.
type hostExecPassthrough struct{}

func (hostExecPassthrough) Run(ctx context.Context, dir, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// TestValidateSkill_Execute_RealTools_StructuredOutput runs ValidateSkill.Execute
// end-to-end against real terraform and tflint binaries (via
// hostExecPassthrough, standing in for a sandbox.Executor) on a deliberately
// broken fixture, proving the end-to-end output is the structured summary —
// concrete file:line + message — rather than a raw JSON dump the model
// would have to parse itself.
//
// The fixture deliberately uses the built-in terraform_data resource type
// (ships with the terraform binary itself, "terraform.io/builtin/terraform")
// instead of a registry provider like aws_instance: `terraform init` for an
// aws_instance fixture has to download the hashicorp/aws provider plugin,
// which was observed taking ~55s in this environment — dangerously close to
// ValidateSkill.Execute's 60s total budget for init+validate combined, and
// unrelated to what this test is actually verifying (JSON parsing, not
// provider download speed). terraform_data still exercises a real
// schema-validated "Unsupported argument" diagnostic with zero network
// dependency.
func TestValidateSkill_Execute_RealTools_StructuredOutput(t *testing.T) {
	requireTerraformAndTflint(t)

	dir := t.TempDir()
	tf := "resource \"terraform_data\" \"example\" {\n" +
		"  input = \"hello\"\n" +
		"  foo   = \"bar\"\n" +
		"}\n\n" +
		"output \"undeclared\" {\n" +
		"  value = var.undeclared_var\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	s := NewValidateSkill(hostExecPassthrough{})
	input, _ := json.Marshal(map[string]any{"path": dir})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(out, "=== tflint ===") || !strings.Contains(out, "=== terraform validate ===") {
		t.Fatalf("expected both section headers, got: %q", out)
	}
	if !strings.Contains(out, "main.tf:3") {
		t.Errorf("expected precise file:line for the unsupported-argument error, got: %q", out)
	}
	if !strings.Contains(out, "Unsupported argument") {
		t.Errorf("expected terraform validate's diagnostic summary, got: %q", out)
	}
	if !strings.Contains(out, "Reference to undeclared input variable") {
		t.Errorf("expected terraform validate's second diagnostic, got: %q", out)
	}
	// The whole point of this change: no raw JSON dump left for the model
	// to parse itself.
	if strings.Contains(out, `"format_version"`) || strings.Contains(out, `"diagnostics"`) {
		t.Errorf("expected structured summary, not a raw terraform validate JSON dump: %q", out)
	}
	if strings.Contains(out, `"issues"`) || strings.Contains(out, `"rule"`) {
		t.Errorf("expected structured summary, not a raw tflint JSON dump: %q", out)
	}
}

// --- sandbox.Executor wiring (no Docker required — a fake stands in) ---

// fakeExecutor is a sandbox.Executor test double that records every call it
// receives instead of shelling out anywhere.
type fakeExecutor struct {
	calls  []fakeCall
	out    string
	errOut string
	err    error
}

type fakeCall struct {
	dir  string
	name string
	args []string
}

func (f *fakeExecutor) Run(_ context.Context, dir, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, fakeCall{dir: dir, name: name, args: args})
	return f.out, f.errOut, f.err
}

// TestValidateSkill_NilExecutor_NoLongerFallsBackToHost proves the host-exec
// fallback path is gone: constructing with a nil executor must not silently
// run tflint/terraform on the host, it must return an error.
func TestValidateSkill_NilExecutor_NoLongerFallsBackToHost(t *testing.T) {
	s := NewValidateSkill(nil)
	dir := t.TempDir()
	input, _ := json.Marshal(map[string]any{"path": dir})
	_, err := s.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected an error when no sandbox executor is configured — host-exec fallback should be removed")
	}
}

// TestSecurityScanSkill_NilExecutor_NoLongerFallsBackToHost is
// TestValidateSkill_NilExecutor_NoLongerFallsBackToHost's SecurityScanSkill
// counterpart.
func TestSecurityScanSkill_NilExecutor_NoLongerFallsBackToHost(t *testing.T) {
	s := NewSecurityScanSkill(nil)
	dir := t.TempDir()
	input, _ := json.Marshal(map[string]any{"path": dir})
	_, err := s.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected an error when no sandbox executor is configured — host-exec fallback should be removed")
	}
}

func TestNewValidateSkill_NilExecutor_MatchesZeroValue(t *testing.T) {
	// NewValidateSkill(nil) must behave exactly like &ValidateSkill{} —
	// both are the nil-executor zero value that Execute treats as a hard
	// error (see TestValidateSkill_NilExecutor_NoLongerFallsBackToHost).
	s := NewValidateSkill(nil)
	if s.executor != nil {
		t.Fatalf("expected nil executor, got %#v", s.executor)
	}
}

func TestValidateSkill_RoutesThroughExecutor(t *testing.T) {
	fe := &fakeExecutor{out: "{}\n", err: nil}
	s := NewValidateSkill(fe)

	dir := t.TempDir()
	input, _ := json.Marshal(map[string]any{
		"path":                   dir,
		"run_tflint":             true,
		"run_terraform_validate": false,
	})
	if _, err := s.Execute(context.Background(), input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call to the executor, got %d: %+v", len(fe.calls), fe.calls)
	}
	call := fe.calls[0]
	if call.dir != dir {
		t.Errorf("dir = %q, want %q", call.dir, dir)
	}
	if call.name != "tflint" {
		t.Errorf("name = %q, want tflint", call.name)
	}
}

func TestNewSecurityScanSkill_NilExecutor_MatchesZeroValue(t *testing.T) {
	s := NewSecurityScanSkill(nil)
	if s.executor != nil {
		t.Fatalf("expected nil executor, got %#v", s.executor)
	}
}

func TestSecurityScanSkill_RoutesThroughExecutor(t *testing.T) {
	fe := &fakeExecutor{
		out: `{"results":{"passed_checks":[],"failed_checks":[]},"summary":{"passed":2,"failed":0}}`,
	}
	s := NewSecurityScanSkill(fe)

	dir := t.TempDir()
	input, _ := json.Marshal(map[string]any{"path": dir})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "2 passed") {
		t.Errorf("expected parsed checkov output, got: %q", out)
	}

	if len(fe.calls) != 1 {
		t.Fatalf("expected 1 call to the executor, got %d: %+v", len(fe.calls), fe.calls)
	}
	call := fe.calls[0]
	if call.dir != dir {
		t.Errorf("dir = %q, want %q (the host path — the executor is responsible for mounting it)", call.dir, dir)
	}
	if call.name != "checkov" {
		t.Errorf("name = %q, want checkov", call.name)
	}
	// scanPath is bind-mounted at /workspace and set as the container's cwd
	// by the executor, so checkov must be pointed at "." rather than the
	// host-absolute dir — passing the host path here would be meaningless
	// inside the container's filesystem.
	foundDot := false
	for i, a := range call.args {
		if a == "-d" && i+1 < len(call.args) && call.args[i+1] == "." {
			foundDot = true
		}
	}
	if !foundDot {
		t.Errorf("expected -d . in sandboxed checkov args, got: %v", call.args)
	}
}

func TestSecurityScanSkill_NilExecutor_ReturnsError(t *testing.T) {
	// Sandboxed execution is mandatory: with no executor configured,
	// SecurityScanSkill must return a clear error rather than falling back
	// to checking for checkov on the host PATH — the host-exec fallback this
	// test used to cover has been removed (see
	// TestSecurityScanSkill_NilExecutor_NoLongerFallsBackToHost).
	s := NewSecurityScanSkill(nil)
	dir := t.TempDir()
	input, _ := json.Marshal(map[string]any{"path": dir})
	_, err := s.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected an error when no sandbox executor is configured")
	}
}

// --- SecurityScanSkill through a real Docker sandbox (regression coverage) ---

// sandboxTestImage is the sandbox image built by `make sandbox-build` from
// docker/sandbox/Dockerfile — see internal/sandbox/docker_test.go's
// identical constant/skip pattern (duplicated here since this is a
// different package and neither exports test helpers to the other).
const sandboxTestImage = "iac-agent-sandbox:latest"

// requireSandboxDocker skips the test if a Docker daemon or the sandbox
// image isn't available, so `go test ./...` stays green for anyone who has
// Docker but hasn't opted into the sandbox yet.
func requireSandboxDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed — skipping sandbox integration test")
	}
	if err := exec.Command("docker", "image", "inspect", sandboxTestImage).Run(); err != nil {
		t.Skipf("sandbox image %s not built — run 'make sandbox-build' to enable this test", sandboxTestImage)
	}
}

// TestSecurityScanSkill_RealDocker_ParsesRealCheckovOutput is the regression
// test for the bug where checkov's guaranteed stderr noise under
// --network=none (it always fails to reach api0.prismacloud.io for its
// guidelines mapping, since the sandbox has no network by design) polluted
// the merged stdout+stderr buffer that runSandboxed used to feed to
// parseCheckovOutput — the JSON payload landed after a WARNI log line and a
// Python traceback, so json.Unmarshal always failed for every sandboxed
// SecurityScan call, regardless of what was being scanned.
//
// Unlike TestSecurityScanSkill_RoutesThroughExecutor (which uses fakeExecutor
// and so never touches real checkov output or real stderr noise), this test
// runs SecurityScanSkill.Execute end-to-end through a real
// sandbox.DockerExecutor against a fixture with a real checkov-flaggable
// misconfiguration, and asserts a real parsed summary comes back — not a
// JSON-parse error.
func TestSecurityScanSkill_RealDocker_ParsesRealCheckovOutput(t *testing.T) {
	requireSandboxDocker(t)

	dir := t.TempDir()
	// A bare aws_s3_bucket with no versioning/encryption/logging/public-access
	// block is a real checkov-flaggable misconfiguration (e.g. CKV_AWS_145,
	// "Ensure that S3 buckets are encrypted with KMS by default") — checkov
	// needs no `terraform init` to flag this, it parses the .tf file statically.
	tf := "resource \"aws_s3_bucket\" \"bad\" {\n  bucket = \"my-insecure-bucket\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	de := sandbox.NewDockerExecutor(sandboxTestImage, "", "")
	s := NewSecurityScanSkill(de)

	input, _ := json.Marshal(map[string]any{"path": dir})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("SecurityScanSkill.Execute through real Docker sandbox failed: %v\noutput: %q", err, out)
	}

	if strings.Contains(out, "parse checkov") {
		t.Fatalf("got a checkov output parse failure instead of a real summary — the merged stdout+stderr bug has resurfaced: %q", out)
	}
	if !strings.Contains(out, "Security Scan:") {
		t.Errorf("expected a parsed Security Scan summary, got: %q", out)
	}
	// The fixture is deliberately unencrypted/unversioned/unlogged, so
	// checkov must report at least one real failed check against it — this
	// is what proves stdout was actually parsed as real JSON, not just that
	// no error happened to bubble up.
	if !strings.Contains(out, "FAILED:") {
		t.Errorf("expected at least one failed check for the deliberately misconfigured fixture, got: %q", out)
	}
}

// --- DriftDetectSkill ---

func TestDriftDetect_Metadata(t *testing.T) {
	s := &DriftDetectSkill{}
	if got := s.Name(); got != "detect_drift" {
		t.Errorf("Name() = %q, want %q", got, "detect_drift")
	}
	if !s.IsReadOnly() {
		t.Error("IsReadOnly() = false, want true")
	}
	if s.IsDestructive(nil) {
		t.Error("IsDestructive() = true, want false")
	}
	if s.Prompt() == "" {
		t.Error("Prompt should be non-empty")
	}
}

func TestDriftDetect_Execute_InvalidInput(t *testing.T) {
	s := &DriftDetectSkill{}
	_, err := s.Execute(context.Background(), json.RawMessage(`{not valid json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON input")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' in error, got: %v", err)
	}
}

func TestDriftDetect_Execute_MissingPath(t *testing.T) {
	s := &DriftDetectSkill{}
	input, _ := json.Marshal(map[string]any{})
	_, err := s.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("expected 'path is required' in error, got: %v", err)
	}
}

// requireTerraform skips the test if the terraform binary isn't on PATH.
// DriftDetectSkill never calls tflint, so it doesn't need
// requireTerraformAndTflint's stricter two-binary gate.
func requireTerraform(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not installed — skipping real-tool integration test")
	}
}

// TestDriftDetect_Execute_NoDrift_RealTerraform proves the "No drift
// detected" happy path against a real terraform binary. It uses the
// built-in terraform_data resource (no provider download, no network —
// same zero-network-dependency convention as
// TestValidateSkill_Execute_RealTools_StructuredOutput above) and a real
// `apply` so the state file genuinely matches the config.
func TestDriftDetect_Execute_NoDrift_RealTerraform(t *testing.T) {
	requireTerraform(t)

	dir := t.TempDir()
	tf := "resource \"terraform_data\" \"example\" {\n  input = \"hello\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	initCmd := exec.Command("terraform", "init", "-input=false", "-no-color")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("terraform init: %v\n%s", err, out)
	}
	applyCmd := exec.Command("terraform", "apply", "-auto-approve", "-input=false", "-no-color")
	applyCmd.Dir = dir
	if out, err := applyCmd.CombinedOutput(); err != nil {
		t.Fatalf("terraform apply: %v\n%s", err, out)
	}

	s := &DriftDetectSkill{}
	input, _ := json.Marshal(map[string]any{"path": dir})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "No drift detected") {
		t.Errorf("expected 'No drift detected', got: %q", out)
	}
}

// TestDriftDetect_Execute_DriftDetected_RealTerraform simulates drift by
// changing the config after a real apply — DriftDetectSkill only ever runs
// `plan`, never `apply`, so a config/state mismatch from either a live
// infra change or a local edit surfaces identically as a plan diff.
func TestDriftDetect_Execute_DriftDetected_RealTerraform(t *testing.T) {
	requireTerraform(t)

	dir := t.TempDir()
	tfPath := filepath.Join(dir, "main.tf")
	original := "resource \"terraform_data\" \"example\" {\n  input = \"hello\"\n}\n"
	if err := os.WriteFile(tfPath, []byte(original), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	initCmd := exec.Command("terraform", "init", "-input=false", "-no-color")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("terraform init: %v\n%s", err, out)
	}
	applyCmd := exec.Command("terraform", "apply", "-auto-approve", "-input=false", "-no-color")
	applyCmd.Dir = dir
	if out, err := applyCmd.CombinedOutput(); err != nil {
		t.Fatalf("terraform apply: %v\n%s", err, out)
	}

	changed := "resource \"terraform_data\" \"example\" {\n  input = \"changed\"\n}\n"
	if err := os.WriteFile(tfPath, []byte(changed), 0o644); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}

	s := &DriftDetectSkill{}
	input, _ := json.Marshal(map[string]any{"path": dir})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Drift detected") {
		t.Errorf("expected 'Drift detected', got: %q", out)
	}
}

// TestDriftDetect_Execute_InitFails_SoftError proves an init failure returns
// a soft (nil-error) message rather than propagating a Go error — an
// invalid backend type fails deterministically with no network dependency.
func TestDriftDetect_Execute_InitFails_SoftError(t *testing.T) {
	requireTerraform(t)

	dir := t.TempDir()
	tf := "terraform {\n  backend \"invalid_backend_type\" {}\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	s := &DriftDetectSkill{}
	input, _ := json.Marshal(map[string]any{"path": dir})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute should return a soft error message, not a Go error: %v", err)
	}
	if !strings.Contains(out, "terraform init failed") {
		t.Errorf("expected soft init-failure message, got: %q", out)
	}
}

// TestDriftDetect_Execute_PlanFails_SoftError_RealTerraform proves the
// "terraform plan failed" soft-error branch (Execute's final return, reached
// when `terraform plan` exits non-zero for a reason OTHER than drift — exit
// code 1, not the exit code 2 that means "changes present") returns a soft
// (nil-error) message rather than propagating a Go error, mirroring
// TestDriftDetect_Execute_InitFails_SoftError above for the sibling branch.
//
// A nonexistent -var-file path is used to fail `plan` deterministically and
// without any network dependency: init succeeds against the same
// zero-network terraform_data fixture used elsewhere in this file, so the
// failure is isolated to the plan step itself.
func TestDriftDetect_Execute_PlanFails_SoftError_RealTerraform(t *testing.T) {
	requireTerraform(t)

	dir := t.TempDir()
	tf := "resource \"terraform_data\" \"example\" {\n  input = \"hello\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	initCmd := exec.Command("terraform", "init", "-input=false", "-no-color")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("terraform init: %v\n%s", err, out)
	}

	s := &DriftDetectSkill{}
	input, _ := json.Marshal(map[string]any{
		"path":     dir,
		"var_file": filepath.Join(dir, "does-not-exist.tfvars"),
	})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute should return a soft error message, not a Go error: %v", err)
	}
	if !strings.Contains(out, "terraform plan failed") {
		t.Errorf("expected soft plan-failure message, got: %q", out)
	}
	if strings.Contains(out, "No drift detected") || strings.Contains(out, "Drift detected") {
		t.Errorf("a genuine plan failure must not be reported as a drift result either way, got: %q", out)
	}
}

// TestValidateSkill_RealDocker_ParsesRealToolOutput proves ValidateSkill's
// sandboxed path produces the same structured output as its direct-host
// path (TestValidateSkill_Execute_RealTools_StructuredOutput above), but
// running terraform/tflint inside the real sandbox container — the
// real-Docker counterpart SecurityScanSkill already has.
func TestValidateSkill_RealDocker_ParsesRealToolOutput(t *testing.T) {
	requireSandboxDocker(t)

	dir := t.TempDir()
	tf := "resource \"terraform_data\" \"example\" {\n" +
		"  input = \"hello\"\n" +
		"  foo   = \"bar\"\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	executor := sandbox.NewDockerExecutor(sandboxTestImage, "", "")
	s := NewValidateSkill(executor)
	input, _ := json.Marshal(map[string]any{"path": dir})
	out, err := s.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Unsupported argument") {
		t.Errorf("expected real terraform validate diagnostic through the sandbox, got: %q", out)
	}
	// "Unsupported argument" also appears verbatim inside RAW terraform
	// validate -json output (in its "summary" field), so the assertion above
	// alone can't tell a real parsed summary apart from a regression that
	// falls back to dumping the raw JSON. Mirror the stronger assertions from
	// the direct-host counterpart, TestValidateSkill_Execute_RealTools_StructuredOutput,
	// to actually distinguish the two.
	if !strings.Contains(out, "main.tf:3") {
		t.Errorf("expected precise file:line for the unsupported-argument error, got: %q", out)
	}
	if !strings.Contains(out, "=== tflint ===") || !strings.Contains(out, "=== terraform validate ===") {
		t.Fatalf("expected both section headers, got: %q", out)
	}
	if strings.Contains(out, `"format_version"`) || strings.Contains(out, `"diagnostics"`) {
		t.Errorf("expected structured summary, not a raw terraform validate JSON dump: %q", out)
	}
	if strings.Contains(out, `"issues"`) || strings.Contains(out, `"rule"`) {
		t.Errorf("expected structured summary, not a raw tflint JSON dump: %q", out)
	}
}
