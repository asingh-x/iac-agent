// Package tfscan provides a lightweight, dependency-free structural scanner
// for Terraform (.tf) files. It is a best-effort heuristic line-based parser
// of top-level HCL block headers (resource/module/variable/output/provider)
// — it does NOT evaluate expressions, resolve variable interpolation, or
// fully parse HCL syntax. It intentionally uses only the standard library
// (bufio/regexp/strings) so it stays fast and has zero new dependencies.
package tfscan

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Summary is a structural summary of a Terraform configuration directory.
type Summary struct {
	Providers    []ProviderRef `json:"providers"`
	Modules      []ModuleRef   `json:"modules"`
	Resources    []ResourceRef `json:"resources"`
	Variables    []NamedRef    `json:"variables"`
	Outputs      []NamedRef    `json:"outputs"`
	FilesScanned int           `json:"files_scanned"`
}

type ProviderRef struct {
	Name string `json:"name"`
	File string `json:"file"` // path relative to the scanned root
	Line int    `json:"line"` // 1-based line number of the block header
}

type ModuleRef struct {
	Name   string `json:"name"`
	Source string `json:"source"` // the module's `source = "..."` value; empty if not found
	File   string `json:"file"`
	Line   int    `json:"line"`
}

type ResourceRef struct {
	Type string `json:"type"`
	Name string `json:"name"`
	File string `json:"file"`
	Line int    `json:"line"`
}

type NamedRef struct {
	Name string `json:"name"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// Directory names that are never descended into, matching the convention
// used by the existing tree-walker in internal/skills/repo_scan.go.
var skipDirNames = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

var (
	resourceHeaderRe = regexp.MustCompile(`^\s*resource\s+"([^"]+)"\s+"([^"]+)"\s*\{`)
	moduleHeaderRe   = regexp.MustCompile(`^\s*module\s+"([^"]+)"\s*\{`)
	variableHeaderRe = regexp.MustCompile(`^\s*variable\s+"([^"]+)"\s*\{`)
	outputHeaderRe   = regexp.MustCompile(`^\s*output\s+"([^"]+)"\s*\{`)
	providerHeaderRe = regexp.MustCompile(`^\s*provider\s+"([^"]+)"\s*\{`)
	sourceAttrRe     = regexp.MustCompile(`^\s*source\s*=\s*"([^"]*)"`)
)

// stripLineComment returns the effective content of a line for the purpose
// of block-header/attribute matching and brace counting. If the line is
// (after leading whitespace) a "#" or "//" line comment, it is treated as
// blank. This does NOT strip a trailing comment that follows real code on
// the same line (e.g. `source = "x" # note`), nor does it handle multi-line
// "/* ... */" block comments — both are documented heuristic gaps for this
// scanner, which targets whole commented-out block headers, not a full HCL
// comment grammar.
func stripLineComment(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
		return ""
	}
	return line
}

// moduleSource scans the body of a module block (lines[headerIdx+1:]) for a
// `source = "..."` attribute, tracking brace depth so nested blocks (e.g. a
// `lifecycle {}` block) don't cause premature termination. It returns the
// first source value found, or "" if none is found before the block closes.
func moduleSource(lines []string, headerIdx int) string {
	header := stripLineComment(lines[headerIdx])
	depth := strings.Count(header, "{") - strings.Count(header, "}")

	source := ""
	for i := headerIdx + 1; i < len(lines) && depth > 0; i++ {
		effective := stripLineComment(lines[i])
		if source == "" {
			if m := sourceAttrRe.FindStringSubmatch(effective); m != nil {
				source = m[1]
			}
		}
		depth += strings.Count(effective, "{") - strings.Count(effective, "}")
	}
	return source
}

// scanFile extracts block references from a single .tf file's contents and
// appends them onto the given Summary. relPath is the file's path relative
// to the scanned root, used to populate the File field of each ref.
func scanFile(summary *Summary, relPath string, contents []byte) {
	// Normalize line endings and split. This is a simple line-based scan;
	// it does not track any state across files.
	text := strings.ReplaceAll(string(contents), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	for i, line := range lines {
		effective := stripLineComment(line)
		if effective == "" {
			continue
		}
		lineNo := i + 1

		if m := resourceHeaderRe.FindStringSubmatch(effective); m != nil {
			summary.Resources = append(summary.Resources, ResourceRef{
				Type: m[1],
				Name: m[2],
				File: relPath,
				Line: lineNo,
			})
			continue
		}
		if m := moduleHeaderRe.FindStringSubmatch(effective); m != nil {
			summary.Modules = append(summary.Modules, ModuleRef{
				Name:   m[1],
				Source: moduleSource(lines, i),
				File:   relPath,
				Line:   lineNo,
			})
			continue
		}
		if m := variableHeaderRe.FindStringSubmatch(effective); m != nil {
			summary.Variables = append(summary.Variables, NamedRef{
				Name: m[1],
				File: relPath,
				Line: lineNo,
			})
			continue
		}
		if m := outputHeaderRe.FindStringSubmatch(effective); m != nil {
			summary.Outputs = append(summary.Outputs, NamedRef{
				Name: m[1],
				File: relPath,
				Line: lineNo,
			})
			continue
		}
		if m := providerHeaderRe.FindStringSubmatch(effective); m != nil {
			summary.Providers = append(summary.Providers, ProviderRef{
				Name: m[1],
				File: relPath,
				Line: lineNo,
			})
			continue
		}
	}
}

// Parse walks dir and extracts a structural Summary from every *.tf file found.
// It is a best-effort structural/heuristic parse of top-level HCL block headers
// (resource/module/variable/output/provider) — it does NOT evaluate expressions,
// resolve variable interpolation, or fully parse HCL syntax. It intentionally
// skips *.tf.json files (JSON-syntax Terraform configs are out of scope for
// this heuristic text scanner).
func Parse(dir string) (Summary, error) {
	summary := Summary{}

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				// The root itself is unreadable/nonexistent: propagate.
				return err
			}
			if d != nil && d.IsDir() {
				// Can't read this directory's contents; skip it rather than
				// failing the whole scan.
				return filepath.SkipDir
			}
			// Can't stat this file; skip it.
			return nil
		}

		if d.IsDir() {
			if path == dir {
				return nil // never skip the scan root itself
			}
			name := d.Name()
			if skipDirNames[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		name := d.Name()
		if strings.HasSuffix(name, ".tf.json") {
			// Out of scope: JSON-syntax Terraform files are not parsed.
			return nil
		}
		if !strings.HasSuffix(name, ".tf") {
			return nil
		}

		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			// Unreadable file (e.g. permissions): skip it, don't fail the scan.
			return nil
		}

		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}

		scanFile(&summary, rel, contents)
		summary.FilesScanned++
		return nil
	})

	if walkErr != nil {
		return Summary{}, fmt.Errorf("tfscan: parse %s: %w", dir, walkErr)
	}

	return summary, nil
}
