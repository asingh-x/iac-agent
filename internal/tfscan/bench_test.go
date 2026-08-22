package tfscan

// Run with: go test -bench=. -run=^$ ./internal/tfscan/...
//
// No fixture repo exists under this package today (its tests all build tiny
// ad-hoc directories inline via writeFile — see tfscan_test.go); a single
// resource in a single file parses too fast to produce a stable signal, so
// this benchmark builds its own "representative" repo: a few hundred blocks
// spread across enough files that the file-walk-and-regex cost is
// measurable, roughly the scale of a real multi-module Terraform project.
//
// See docs/benchmarks.md for how to run this and what the numbers mean.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	benchRepoFiles            = 20
	benchRepoResourcesPerFile = 10
)

// buildBenchRepo writes a synthetic, "representative-sized" Terraform repo
// into dir: benchRepoFiles files, each with benchRepoResourcesPerFile
// resources plus a module/variable/output block, exercising every block type
// Parse recognizes.
func buildBenchRepo(b *testing.B, dir string) {
	b.Helper()
	for f := 0; f < benchRepoFiles; f++ {
		var sb strings.Builder
		for r := 0; r < benchRepoResourcesPerFile; r++ {
			fmt.Fprintf(&sb, "resource \"aws_instance\" \"web_%d_%d\" {\n  ami           = \"ami-%d\"\n  instance_type = \"t3.micro\"\n  tags = {\n    Name = \"web-%d-%d\"\n  }\n}\n\n", f, r, r, f, r)
		}
		fmt.Fprintf(&sb, "module \"vpc_%d\" {\n  source = \"./modules/vpc\"\n  cidr   = \"10.%d.0.0/16\"\n}\n\n", f, f)
		fmt.Fprintf(&sb, "variable \"region_%d\" {\n  type    = string\n  default = \"us-east-1\"\n}\n\n", f)
		fmt.Fprintf(&sb, "output \"instance_ids_%d\" {\n  value = []\n}\n\n", f)
		fmt.Fprintf(&sb, "provider \"aws\" {\n  alias  = \"p%d\"\n  region = \"us-west-2\"\n}\n", f)

		path := filepath.Join(dir, fmt.Sprintf("main_%d.tf", f))
		if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
			b.Fatalf("write %s: %v", path, err)
		}
	}
}

// BenchmarkParse quantifies the cold-parse cost that RepoScanSkill's
// repo-index cache (internal/skills/repo_scan.go) exists to avoid paying on
// every task for an unchanged repo — see
// internal/skills/repo_scan_bench_test.go for the direct cold-vs-cache-hit
// comparison this number feeds into.
func BenchmarkParse(b *testing.B) {
	dir := b.TempDir()
	buildBenchRepo(b, dir)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(dir); err != nil {
			b.Fatalf("Parse: %v", err)
		}
	}
}
