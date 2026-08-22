package skills

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/tfscan"
)

//go:embed prompts/repo_scan.md
var repoScanPrompt string

// repoIndexCacheTotal tracks repo structural-index cache lookups by result,
// so the token/latency savings from reusing a previous scan are measurable
// rather than just claimed.
var repoIndexCacheTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "tfagent_repo_index_cache_total",
	Help: "Repo structural-index cache lookups, by result.",
}, []string{"result"}) // hit | miss | unavailable

// RepoScanSkill scans a repository and returns a summary of its structure.
//
// When store is non-nil and the target directory is a git repository, it also
// caches a Terraform structural summary (resources/modules/variables/outputs)
// keyed by (remote URL or path, commit sha), so a repeat scan of the same
// commit reuses the previous parse instead of re-reading every .tf file —
// this is the mechanism that avoids re-comprehending an unchanged repo on
// every task. When store is nil (e.g. an isolated sub-agent) or the directory
// isn't a git repo, it falls back to parsing fresh every time with no cache.
type RepoScanSkill struct {
	store db.Store
}

// NewRepoScanSkill creates a RepoScanSkill backed by store for caching.
// Pass nil to disable caching (always parse fresh).
func NewRepoScanSkill(store db.Store) *RepoScanSkill {
	return &RepoScanSkill{store: store}
}

func (s *RepoScanSkill) Name() string                         { return "repo_scan" }
func (s *RepoScanSkill) IsReadOnly() bool                     { return true }
func (s *RepoScanSkill) IsDestructive(_ json.RawMessage) bool { return false }
func (s *RepoScanSkill) Prompt() string                       { return repoScanPrompt }

func (s *RepoScanSkill) Description() string {
	return "Scan a repository directory and return a structural summary: a file/directory tree, plus a Terraform structural index (resources, modules, variables, outputs, providers) that is cached per-commit so repeat scans of the same commit are instant and don't re-read files."
}

func (s *RepoScanSkill) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Root directory to scan (default: current working directory)"
			},
			"max_depth": {
				"type": "integer",
				"description": "Maximum directory depth to traverse (default 3)"
			}
		}
	}`)
}

func (s *RepoScanSkill) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path     string `json:"path"`
		MaxDepth int    `json:"max_depth"`
	}
	_ = json.Unmarshal(input, &args)

	root := args.Path
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("repo_scan: getwd: %w", err)
		}
	}

	maxDepth := args.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 3
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Repository: %s\n\n", root)

	if err := walk(&sb, root, root, 0, maxDepth); err != nil {
		return sb.String(), err
	}

	sb.WriteString("\n")
	sb.WriteString(s.terraformSummary(ctx, root))

	return sb.String(), nil
}

// terraformSummary returns a rendered Terraform structural summary for root,
// reusing a cached parse when one exists for the current commit.
func (s *RepoScanSkill) terraformSummary(ctx context.Context, root string) string {
	repoID, sha, ok := gitIdentity(root)
	if !ok || s.store == nil {
		repoIndexCacheTotal.WithLabelValues("unavailable").Inc()
		summary, err := tfscan.Parse(root)
		if err != nil {
			return ""
		}
		return renderTerraformSummary(summary, "")
	}

	if entry, err := s.store.GetRepoIndex(ctx, repoID, sha); err == nil && entry != nil {
		var summary tfscan.Summary
		if err := json.Unmarshal(entry.Summary, &summary); err == nil {
			repoIndexCacheTotal.WithLabelValues("hit").Inc()
			note := fmt.Sprintf("cached index from commit %s, indexed %s — no files re-read",
				shortSHA(sha), entry.IndexedAt.UTC().Format(time.RFC3339))
			return renderTerraformSummary(summary, note)
		}
	}

	repoIndexCacheTotal.WithLabelValues("miss").Inc()
	summary, err := tfscan.Parse(root)
	if err != nil {
		return ""
	}
	if raw, err := json.Marshal(summary); err == nil {
		_ = s.store.SaveRepoIndex(ctx, repoID, sha, raw)
	}
	return renderTerraformSummary(summary, "")
}

// gitIdentity resolves a stable (repoID, commitSHA) pair for dir. repoID
// prefers the "origin" remote URL (stable across worktrees/clones); if dir
// isn't a git repo or has no commits yet, ok is false and callers must not
// attempt to cache.
func gitIdentity(dir string) (repoID, commitSHA string, ok bool) {
	sha, err := runGit(dir, "rev-parse", "HEAD")
	if err != nil || sha == "" {
		return "", "", false
	}
	repoID, err = runGit(dir, "remote", "get-url", "origin")
	if err != nil || repoID == "" {
		// No remote configured — fall back to the absolute path as identity.
		// Caching still works within this filesystem, just not portably
		// across clones of the same remote.
		abs, absErr := filepath.Abs(dir)
		if absErr != nil {
			return "", "", false
		}
		repoID = abs
	}
	return repoID, sha, true
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// renderTerraformSummary formats a tfscan.Summary as concise text for the
// agent. note, if non-empty, is appended describing the summary's provenance
// (e.g. cache hit vs fresh parse).
func renderTerraformSummary(summary tfscan.Summary, note string) string {
	if summary.FilesScanned == 0 {
		return "Terraform index: no .tf files found.\n"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Terraform index (%d .tf files):\n", summary.FilesScanned)

	if len(summary.Providers) > 0 {
		names := make([]string, len(summary.Providers))
		for i, p := range summary.Providers {
			names[i] = p.Name
		}
		fmt.Fprintf(&sb, "  Providers: %s\n", strings.Join(names, ", "))
	}

	if len(summary.Modules) > 0 {
		fmt.Fprintf(&sb, "  Modules (%d):\n", len(summary.Modules))
		for _, m := range summary.Modules {
			if m.Source != "" {
				fmt.Fprintf(&sb, "    - %s -> %s (%s:%d)\n", m.Name, m.Source, m.File, m.Line)
			} else {
				fmt.Fprintf(&sb, "    - %s (%s:%d)\n", m.Name, m.File, m.Line)
			}
		}
	}

	if len(summary.Resources) > 0 {
		fmt.Fprintf(&sb, "  Resources (%d):\n", len(summary.Resources))
		for _, r := range summary.Resources {
			fmt.Fprintf(&sb, "    - %s.%s (%s:%d)\n", r.Type, r.Name, r.File, r.Line)
		}
	}

	if len(summary.Variables) > 0 {
		names := make([]string, len(summary.Variables))
		for i, v := range summary.Variables {
			names[i] = v.Name
		}
		fmt.Fprintf(&sb, "  Variables (%d): %s\n", len(names), strings.Join(names, ", "))
	}

	if len(summary.Outputs) > 0 {
		names := make([]string, len(summary.Outputs))
		for i, o := range summary.Outputs {
			names[i] = o.Name
		}
		fmt.Fprintf(&sb, "  Outputs (%d): %s\n", len(names), strings.Join(names, ", "))
	}

	if note != "" {
		fmt.Fprintf(&sb, "  (%s)\n", note)
	}

	return sb.String()
}

func walk(sb *strings.Builder, root, path string, depth, maxDepth int) error {
	if depth > maxDepth {
		return nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}

	indent := strings.Repeat("  ", depth)
	for _, e := range entries {
		name := e.Name()
		// Skip hidden dirs/files at depth > 0 (allow .git at top level for info).
		if strings.HasPrefix(name, ".") && depth > 0 {
			continue
		}
		if name == "node_modules" || name == "vendor" || name == ".git" {
			if e.IsDir() {
				fmt.Fprintf(sb, "%s%s/ (skipped)\n", indent, name)
			}
			continue
		}

		rel, _ := filepath.Rel(root, filepath.Join(path, name))
		if e.IsDir() {
			fmt.Fprintf(sb, "%s%s/\n", indent, name)
			_ = walk(sb, root, filepath.Join(path, name), depth+1, maxDepth)
		} else {
			info, _ := e.Info()
			size := ""
			if info != nil {
				size = fmt.Sprintf(" (%d bytes)", info.Size())
			}
			_ = rel
			fmt.Fprintf(sb, "%s%s%s\n", indent, name, size)
		}
	}
	return nil
}
