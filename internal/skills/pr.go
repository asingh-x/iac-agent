package skills

import (
	_ "embed"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tf-agent/tf-agent/internal/taskctx"
)

//go:embed prompts/pr.md
var prPrompt string

const githubAPIBase = "https://api.github.com"

// CreatePRSkill creates a GitHub PR for generated Terraform files.
// Input JSON: {"repo_url": string, "branch": string, "title": string, "body": string, "files": {"path": "content"}}
type CreatePRSkill struct {
	baseURL string // empty = githubAPIBase (production); overridable in tests
}

func (s *CreatePRSkill) Name() string                         { return "CreatePR" }
func (s *CreatePRSkill) IsReadOnly() bool                     { return false }
func (s *CreatePRSkill) IsDestructive(_ json.RawMessage) bool { return false }
func (s *CreatePRSkill) Prompt() string                       { return prPrompt }

func (s *CreatePRSkill) apiBase() string {
	if s.baseURL != "" {
		return s.baseURL
	}
	return githubAPIBase
}

func (s *CreatePRSkill) Description() string {
	return "Create a GitHub pull request with the provided files. Requires GITHUB_TOKEN."
}

func (s *CreatePRSkill) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo_url": {
				"type": "string",
				"description": "Repository URL e.g. github.com/org/repo"
			},
			"branch": {
				"type": "string",
				"description": "Name of the branch to create"
			},
			"title": {
				"type": "string",
				"description": "PR title"
			},
			"body": {
				"type": "string",
				"description": "PR description body"
			},
			"files": {
				"type": "object",
				"description": "Map of file paths to file contents",
				"additionalProperties": {"type": "string"}
			}
		},
		"required": ["repo_url", "branch", "title", "body", "files"]
	}`)
}

func (s *CreatePRSkill) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		RepoURL string            `json:"repo_url"`
		Branch  string            `json:"branch"`
		Title   string            `json:"title"`
		Body    string            `json:"body"`
		Files   map[string]string `json:"files"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("CreatePR: invalid input: %w", err)
	}

	// Credentials: task context → env var.
	token := ""
	if creds, ok := taskctx.FromContext(ctx); ok && creds.GitHubToken != "" {
		token = creds.GitHubToken
	}
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		return "", fmt.Errorf("CreatePR: github_token is required (pass in task request or set GITHUB_TOKEN)")
	}

	owner, repo, err := parseRepoURL(args.RepoURL)
	if err != nil {
		return "", fmt.Errorf("CreatePR: %w", err)
	}

	// Derive deterministic branch name from TaskID if available.
	branch := args.Branch
	if creds, ok := taskctx.FromContext(ctx); ok && creds.TaskID != "" {
		branch = "iac-agent/task-" + creds.TaskID
	}

	client := &http.Client{Timeout: 30 * time.Second}
	gh := &githubClient{client: client, token: token, owner: owner, repo: repo, baseURL: s.apiBase()}

	// Step a: get base SHA from main branch.
	baseSHA, err := gh.getRef(ctx, "heads/main")
	if err != nil {
		return "", fmt.Errorf("CreatePR: get main ref: %w", err)
	}

	// Step b: create branch.
	if err := gh.createRef(ctx, "refs/heads/"+branch, baseSHA); err != nil {
		return "", fmt.Errorf("CreatePR: create branch: %w", err)
	}

	// Step c: create/update each file.
	for path, content := range args.Files {
		if err := gh.createOrUpdateFile(ctx, path, content, branch); err != nil {
			return "", fmt.Errorf("CreatePR: upload file %s: %w", path, err)
		}
	}

	// Step d: open PR.
	prURL, err := gh.createPR(ctx, args.Title, args.Body, branch, "main")
	if err != nil {
		return "", fmt.Errorf("CreatePR: create PR: %w", err)
	}

	return fmt.Sprintf("Pull request created: %s", prURL), nil
}

func parseRepoURL(repoURL string) (owner, repo string, err error) {
	// Accept github.com/owner/repo or https://github.com/owner/repo
	repoURL = strings.TrimPrefix(repoURL, "https://")
	repoURL = strings.TrimPrefix(repoURL, "http://")
	repoURL = strings.TrimPrefix(repoURL, "github.com/")
	parts := strings.SplitN(repoURL, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo_url %q — expected github.com/owner/repo", repoURL)
	}
	return parts[0], parts[1], nil
}

type githubClient struct {
	client  *http.Client
	token   string
	owner   string
	repo    string
	baseURL string
}

func (g *githubClient) do(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 500_000))
	return respBody, resp.StatusCode, err
}

func (g *githubClient) getRef(ctx context.Context, ref string) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/git/ref/%s", g.owner, g.repo, ref)
	body, status, err := g.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GitHub API %s returned %d: %s", path, status, string(body))
	}
	var result struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse ref response: %w", err)
	}
	return result.Object.SHA, nil
}

func (g *githubClient) createRef(ctx context.Context, ref, sha string) error {
	path := fmt.Sprintf("/repos/%s/%s/git/refs", g.owner, g.repo)
	payload := map[string]string{"ref": ref, "sha": sha}
	body, status, err := g.do(ctx, http.MethodPost, path, payload)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("GitHub API %s returned %d: %s", path, status, string(body))
	}
	return nil
}

func (g *githubClient) createOrUpdateFile(ctx context.Context, filePath, content, branch string) error {
	path := fmt.Sprintf("/repos/%s/%s/contents/%s", g.owner, g.repo, filePath)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))

	// Check if file exists to get its SHA for updates.
	existingBody, existingStatus, err := g.do(ctx, http.MethodGet, path+"?ref="+branch, nil)
	if err != nil {
		return err
	}

	payload := map[string]interface{}{
		"message": "chore: add " + filePath,
		"content": encoded,
		"branch":  branch,
	}

	if existingStatus == http.StatusOK {
		var existing struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(existingBody, &existing); err == nil && existing.SHA != "" {
			payload["sha"] = existing.SHA
		}
	}

	body, status, err := g.do(ctx, http.MethodPut, path, payload)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("GitHub API %s returned %d: %s", path, status, string(body))
	}
	return nil
}

func (g *githubClient) createPR(ctx context.Context, title, prBody, head, base string) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls", g.owner, g.repo)
	payload := map[string]string{
		"title": title,
		"body":  prBody,
		"head":  head,
		"base":  base,
	}
	body, status, err := g.do(ctx, http.MethodPost, path, payload)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("GitHub API %s returned %d: %s", path, status, string(body))
	}
	var result struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse PR response: %w", err)
	}
	return result.HTMLURL, nil
}
