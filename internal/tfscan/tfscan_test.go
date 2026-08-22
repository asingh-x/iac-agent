package tfscan

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestParse_OneOfEachBlockType(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_s3_bucket" "main" {
  bucket = "my-bucket"
}

module "network" {
  source = "./modules/network"
  cidr   = "10.0.0.0/16"
}

variable "region" {
  type = string
}

output "bucket_arn" {
  value = aws_s3_bucket.main.arn
}

provider "aws" {
  region = "us-east-1"
}
`)

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got.FilesScanned != 1 {
		t.Fatalf("FilesScanned = %d, want 1", got.FilesScanned)
	}

	if len(got.Resources) != 1 {
		t.Fatalf("Resources = %+v, want 1 entry", got.Resources)
	}
	r := got.Resources[0]
	if r.Type != "aws_s3_bucket" || r.Name != "main" || r.File != "main.tf" || r.Line != 1 {
		t.Errorf("Resource = %+v, want {aws_s3_bucket main main.tf 1}", r)
	}

	if len(got.Modules) != 1 {
		t.Fatalf("Modules = %+v, want 1 entry", got.Modules)
	}
	m := got.Modules[0]
	if m.Name != "network" || m.Source != "./modules/network" || m.File != "main.tf" || m.Line != 5 {
		t.Errorf("Module = %+v, want {network ./modules/network main.tf 5}", m)
	}

	if len(got.Variables) != 1 {
		t.Fatalf("Variables = %+v, want 1 entry", got.Variables)
	}
	v := got.Variables[0]
	if v.Name != "region" || v.File != "main.tf" || v.Line != 10 {
		t.Errorf("Variable = %+v, want {region main.tf 10}", v)
	}

	if len(got.Outputs) != 1 {
		t.Fatalf("Outputs = %+v, want 1 entry", got.Outputs)
	}
	o := got.Outputs[0]
	if o.Name != "bucket_arn" || o.File != "main.tf" || o.Line != 14 {
		t.Errorf("Output = %+v, want {bucket_arn main.tf 14}", o)
	}

	if len(got.Providers) != 1 {
		t.Fatalf("Providers = %+v, want 1 entry", got.Providers)
	}
	p := got.Providers[0]
	if p.Name != "aws" || p.File != "main.tf" || p.Line != 18 {
		t.Errorf("Provider = %+v, want {aws main.tf 18}", p)
	}
}

func TestParse_MultipleResourcesSameFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "resources.tf", `resource "aws_instance" "web" {
  ami = "ami-123"
}

resource "aws_instance" "db" {
  ami = "ami-456"
}

resource "aws_s3_bucket" "logs" {
  bucket = "logs"
}
`)

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Resources) != 3 {
		t.Fatalf("Resources = %+v, want 3 entries", got.Resources)
	}
	want := []ResourceRef{
		{Type: "aws_instance", Name: "web", File: "resources.tf", Line: 1},
		{Type: "aws_instance", Name: "db", File: "resources.tf", Line: 5},
		{Type: "aws_s3_bucket", Name: "logs", File: "resources.tf", Line: 9},
	}
	for i, w := range want {
		if got.Resources[i] != w {
			t.Errorf("Resources[%d] = %+v, want %+v", i, got.Resources[i], w)
		}
	}
}

func TestParse_ModuleSourceAfterOtherAttributes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `module "vpc" {
  cidr_block = "10.0.0.0/16"
  tags = {
    env = "prod"
  }
  source = "./modules/vpc"
  enable_nat = true
}
`)

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Modules) != 1 {
		t.Fatalf("Modules = %+v, want 1 entry", got.Modules)
	}
	if got.Modules[0].Source != "./modules/vpc" {
		t.Errorf("Module.Source = %q, want %q", got.Modules[0].Source, "./modules/vpc")
	}
	if got.Modules[0].Name != "vpc" || got.Modules[0].Line != 1 {
		t.Errorf("Module = %+v, want name=vpc line=1", got.Modules[0])
	}
}

func TestParse_ModuleWithoutSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `module "no_source" {
  cidr_block = "10.0.0.0/16"
}
`)
	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Modules) != 1 {
		t.Fatalf("Modules = %+v, want 1 entry", got.Modules)
	}
	if got.Modules[0].Source != "" {
		t.Errorf("Module.Source = %q, want empty", got.Modules[0].Source)
	}
}

func TestParse_CommentedOutResourceNotCounted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `# resource "aws_instance" "old" {
#   ami = "ami-old"
# }

// resource "aws_instance" "also_old" {
//   ami = "ami-also-old"
// }

resource "aws_instance" "current" {
  ami = "ami-current"
}
`)

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Resources) != 1 {
		t.Fatalf("Resources = %+v, want 1 entry (commented ones excluded)", got.Resources)
	}
	if got.Resources[0].Name != "current" {
		t.Errorf("Resources[0].Name = %q, want current", got.Resources[0].Name)
	}
}

func TestParse_NonTfFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "terraform.tfvars", `region = "us-east-1"`)
	writeFile(t, dir, "README.md", `# resource "fake" "fake" {`)
	writeFile(t, dir, "generated.tf.json", `{"resource": {}}`)
	writeFile(t, dir, "main.tf", `resource "aws_instance" "real" {
  ami = "ami-real"
}
`)

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FilesScanned != 1 {
		t.Fatalf("FilesScanned = %d, want 1 (only main.tf)", got.FilesScanned)
	}
	if len(got.Resources) != 1 || got.Resources[0].Name != "real" {
		t.Fatalf("Resources = %+v, want just 'real'", got.Resources)
	}
}

func TestParse_SkipsIgnoredDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.tf", `resource "aws_instance" "top" {
  ami = "ami-top"
}
`)
	writeFile(t, dir, ".git/config", "should not be read as tf")
	writeFile(t, dir, ".git/fake.tf", `resource "aws_instance" "in_git" {}`)
	writeFile(t, dir, "node_modules/pkg/fake.tf", `resource "aws_instance" "in_node_modules" {}`)
	writeFile(t, dir, "vendor/pkg/fake.tf", `resource "aws_instance" "in_vendor" {}`)
	writeFile(t, dir, ".hidden/fake.tf", `resource "aws_instance" "in_hidden" {}`)

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FilesScanned != 1 {
		t.Fatalf("FilesScanned = %d, want 1 (skip dirs excluded)", got.FilesScanned)
	}
	if len(got.Resources) != 1 || got.Resources[0].Name != "top" {
		t.Fatalf("Resources = %+v, want just 'top'", got.Resources)
	}
}

func TestParse_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FilesScanned != 0 {
		t.Errorf("FilesScanned = %d, want 0", got.FilesScanned)
	}
	if len(got.Resources) != 0 || len(got.Modules) != 0 || len(got.Variables) != 0 ||
		len(got.Outputs) != 0 || len(got.Providers) != 0 {
		t.Errorf("expected all-empty Summary, got %+v", got)
	}
}

func TestParse_NonexistentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := Parse(dir)
	if err == nil {
		t.Fatal("Parse: expected error for nonexistent directory, got nil")
	}
}

func TestParse_UnreadableFileIsSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits behave differently on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permission checks")
	}

	dir := t.TempDir()
	writeFile(t, dir, "readable.tf", `resource "aws_instance" "ok" {
  ami = "ami-ok"
}
`)
	unreadable := writeFile(t, dir, "unreadable.tf", `resource "aws_instance" "nope" {
  ami = "ami-nope"
}
`)
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FilesScanned != 1 {
		t.Fatalf("FilesScanned = %d, want 1 (unreadable file skipped)", got.FilesScanned)
	}
	if len(got.Resources) != 1 || got.Resources[0].Name != "ok" {
		t.Fatalf("Resources = %+v, want just 'ok'", got.Resources)
	}
}

func TestParse_FilesScannedCountsFilesWithZeroMatches(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "empty.tf", `# just a comment, no blocks here
`)
	writeFile(t, dir, "vars.tf", `variable "a" {}
variable "b" {}
`)
	writeFile(t, dir, "sub/more.tf", `output "x" {
  value = 1
}
`)

	got, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.FilesScanned != 3 {
		t.Fatalf("FilesScanned = %d, want 3", got.FilesScanned)
	}
	if len(got.Variables) != 2 {
		t.Fatalf("Variables = %+v, want 2", got.Variables)
	}
	if len(got.Outputs) != 1 {
		t.Fatalf("Outputs = %+v, want 1", got.Outputs)
	}
}
