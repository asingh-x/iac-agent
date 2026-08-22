package skills

// Run with: go test -bench=. -run=^$ ./internal/skills/...
//
// These quantify the repo-index cache's real, verified benefit
// (repo_scan.go's terraformSummary): the wall-clock cost of a cold
// tfscan.Parse over a "representative-sized" repo versus a cache hit
// (GetRepoIndex + json.Unmarshal, no .tf files read at all).
//
// This deliberately benchmarks those two primitives directly rather than the
// full terraformSummary(ctx, root): that function's first line always calls
// gitIdentity(root), which shells out to `git` twice (rev-parse HEAD, remote
// get-url origin) — a real cost, but a roughly constant one paid on every
// call regardless of cache hit/miss, and one that (on this machine) swamps
// the actual Parse-vs-cache-hit difference by 1-2 orders of magnitude if left
// in. Keying the cache-hit benchmark's store lookup on a plain constant
// instead of a git commit sha removes that noise and isolates the number
// that's actually interesting here.
//
// See docs/BENCHMARKS.md for how to run this and what the numbers mean.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tf-agent/tf-agent/internal/db"
	"github.com/tf-agent/tf-agent/internal/tfscan"
)

const (
	benchRepoFiles            = 20
	benchRepoResourcesPerFile = 10
)

// buildBenchRepoFiles writes a synthetic, "representative-sized" Terraform
// repo into dir — the same shape as internal/tfscan/bench_test.go's
// buildBenchRepo (kept as its own small copy rather than an exported helper:
// testing.B/T don't share an interface worth contorting either package's
// test-only code around for one call site each).
func buildBenchRepoFiles(b *testing.B, dir string) {
	b.Helper()
	for f := 0; f < benchRepoFiles; f++ {
		var sb strings.Builder
		for r := 0; r < benchRepoResourcesPerFile; r++ {
			fmt.Fprintf(&sb, "resource \"aws_instance\" \"web_%d_%d\" {\n  ami           = \"ami-%d\"\n  instance_type = \"t3.micro\"\n}\n\n", f, r, r)
		}
		fmt.Fprintf(&sb, "module \"vpc_%d\" {\n  source = \"./modules/vpc\"\n  cidr   = \"10.%d.0.0/16\"\n}\n\n", f, f)
		fmt.Fprintf(&sb, "variable \"region_%d\" {\n  type = string\n}\n\n", f)
		fmt.Fprintf(&sb, "output \"instance_ids_%d\" {\n  value = []\n}\n", f)

		path := filepath.Join(dir, fmt.Sprintf("main_%d.tf", f))
		if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
			b.Fatalf("write %s: %v", path, err)
		}
	}
}

// BenchmarkRepoScan_ColdParse is the baseline the repo-index cache exists to
// avoid paying repeatedly: a fresh tfscan.Parse over a representative-sized
// repo, exactly what terraformSummary falls back to on a cache miss (or when
// store is nil, or the directory isn't a git repo).
func BenchmarkRepoScan_ColdParse(b *testing.B) {
	dir := b.TempDir()
	buildBenchRepoFiles(b, dir)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tfscan.Parse(dir); err != nil {
			b.Fatalf("Parse: %v", err)
		}
	}
}

// BenchmarkRepoScan_CacheHit measures the other half of terraformSummary's
// cache-hit branch: reading back an already-saved summary (GetRepoIndex) and
// unmarshaling it — the work a repeat scan of an unchanged commit does
// INSTEAD OF a fresh Parse.
func BenchmarkRepoScan_CacheHit(b *testing.B) {
	dir := b.TempDir()
	buildBenchRepoFiles(b, dir)

	summary, err := tfscan.Parse(dir)
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		b.Fatalf("Marshal: %v", err)
	}

	store := db.NewMemoryStore()
	const repoID, sha = "bench-repo", "bench-sha"
	if err := store.SaveRepoIndex(context.Background(), repoID, sha, raw); err != nil {
		b.Fatalf("SaveRepoIndex: %v", err)
	}

	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry, err := store.GetRepoIndex(ctx, repoID, sha)
		if err != nil || entry == nil {
			b.Fatalf("GetRepoIndex: %v (entry=%v)", err, entry)
		}
		var got tfscan.Summary
		if err := json.Unmarshal(entry.Summary, &got); err != nil {
			b.Fatalf("Unmarshal: %v", err)
		}
	}
}
