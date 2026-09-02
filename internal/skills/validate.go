package skills

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tf-agent/tf-agent/internal/sandbox"
)

//go:embed prompts/validate.md
var validatePrompt string

// ValidateSkill runs tflint and/or terraform validate on a directory.
type ValidateSkill struct {
	// executor routes tflint/terraform invocations through a sandbox.Executor
	// (e.g. sandbox.DockerExecutor) — sandboxed execution is mandatory, so a
	// nil executor (the zero value, and what &ValidateSkill{} still gives
	// you) makes Execute return an error rather than running anything on the
	// host. The field stays settable directly for tests that supply a fake
	// executor.
	executor sandbox.Executor
}

// NewValidateSkill builds a ValidateSkill. executor (built from
// config.ServerConfig.SandboxImage/SandboxBackend/...) must be non-nil in
// production — passing nil is supported only so tests can construct a
// ValidateSkill and separately exercise the "no sandbox executor configured"
// error path.
func NewValidateSkill(executor sandbox.Executor) *ValidateSkill {
	return &ValidateSkill{executor: executor}
}

func (s *ValidateSkill) Name() string                         { return "validate_terraform" }
func (s *ValidateSkill) IsReadOnly() bool                     { return true }
func (s *ValidateSkill) IsDestructive(_ json.RawMessage) bool { return false }
func (s *ValidateSkill) Prompt() string                       { return validatePrompt }

func (s *ValidateSkill) Description() string {
	return "Run tflint and terraform validate on Terraform files. Returns lint/validation output."
}

func (s *ValidateSkill) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Directory containing .tf files to validate"
			},
			"run_tflint": {
				"type": "boolean",
				"description": "Run tflint (default true)"
			},
			"run_terraform_validate": {
				"type": "boolean",
				"description": "Run terraform validate (default true)"
			}
		},
		"required": ["path"]
	}`)
}

func (s *ValidateSkill) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path                 string `json:"path"`
		RunTflint            *bool  `json:"run_tflint"`
		RunTerraformValidate *bool  `json:"run_terraform_validate"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("validate_terraform: invalid input: %w", err)
	}

	if s.executor == nil {
		return "", fmt.Errorf("validate_terraform: no sandbox executor configured")
	}

	runTflint := true
	if args.RunTflint != nil {
		runTflint = *args.RunTflint
	}
	runTFValidate := true
	if args.RunTerraformValidate != nil {
		runTFValidate = *args.RunTerraformValidate
	}

	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	type result struct {
		label string
		out   string
	}

	var wg sync.WaitGroup
	ch := make(chan result, 2)

	if runTflint {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := s.runCmd(ctx2, args.Path, "tflint", "--format=json")
			ch <- result{"tflint", formatTflintResult(out, err) + "\n"}
		}()
	}

	if runTFValidate {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// terraform init first (quiet).
			_, _ = s.runCmd(ctx2, args.Path, "terraform", "init", "-backend=false", "-no-color")
			out, err := s.runCmd(ctx2, args.Path, "terraform", "validate", "-json", "-no-color")
			ch <- result{"terraform validate", formatTerraformValidateResult(out, err) + "\n"}
		}()
	}

	wg.Wait()
	close(ch)

	// Collect results; preserve stable ordering (tflint before terraform validate).
	results := map[string]string{}
	for r := range ch {
		results[r.label] = r.out
	}

	var combined string
	if runTflint {
		combined += "=== tflint ===\n" + results["tflint"]
	}
	if runTFValidate {
		combined += "=== terraform validate ===\n" + results["terraform validate"]
	}
	if combined == "" {
		combined = "No validation tools ran."
	}
	return combined, nil
}

// runCmd dispatches to s.executor. Sandboxed execution is mandatory —
// Execute returns early with an error before this is ever called with a nil
// executor.
func (s *ValidateSkill) runCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	stdout, stderr, err := s.executor.Run(ctx, dir, name, args...)
	// Concatenate to preserve this skill's original merged-output display
	// behavior exactly. Both tflint and terraform validate write their
	// -json/--format=json payload to stdout only, so the caller's
	// best-effort JSON parsing (formatTflintResult/
	// formatTerraformValidateResult) still finds valid JSON at the start
	// of this combined string in the normal case; any stderr noise just
	// trails after it, unparsed.
	return stdout + stderr, err
}

// --- terraform validate -json parsing ---
//
// Real shape observed by running `terraform validate -json -no-color`
// (Terraform v1.11.4) against a deliberately broken fixture:
//
//	{
//	  "format_version": "1.0",
//	  "valid": false,
//	  "error_count": 2,
//	  "warning_count": 0,
//	  "diagnostics": [
//	    {
//	      "severity": "error",
//	      "summary": "Unsupported argument",
//	      "detail": "An argument named \"foo\" is not expected here.",
//	      "range": {
//	        "filename": "main.tf",
//	        "start": {"line": 4, "column": 3, "byte": 98},
//	        "end": {"line": 4, "column": 6, "byte": 101}
//	      },
//	      "snippet": { ... }
//	    }
//	  ]
//	}
//
// This matches the brief's sketch closely; the only addition is a "snippet"
// field per-diagnostic (source context/highlight offsets) which isn't needed
// for a compact summary and is intentionally not decoded here.

type tfValidateRange struct {
	Filename string `json:"filename"`
	Start    struct {
		Line int `json:"line"`
	} `json:"start"`
}

type tfValidateDiagnostic struct {
	Severity string          `json:"severity"`
	Summary  string          `json:"summary"`
	Detail   string          `json:"detail"`
	Range    tfValidateRange `json:"range"`
}

type tfValidateJSON struct {
	Valid        bool                   `json:"valid"`
	ErrorCount   int                    `json:"error_count"`
	WarningCount int                    `json:"warning_count"`
	Diagnostics  []tfValidateDiagnostic `json:"diagnostics"`
}

// parseTerraformValidateJSON turns terraform validate -json's output into a
// compact, structured summary: one line per diagnostic in the form
// "<severity>: <file>:<line>: <summary> — <detail>", so both the model
// driving the fix-and-retry loop in prompts/validate.md and a human skimming
// the SSE stream can act on it directly instead of re-parsing raw JSON.
//
// Returns an error if data isn't the JSON shape terraform validate -json
// produces — the caller (formatTerraformValidateResult) falls back to
// showing the raw text in that case rather than hiding a real failure (e.g.
// terraform not installed, or a crash that dumped plain text instead of
// JSON) behind a parse error.
func parseTerraformValidateJSON(data []byte) (string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("terraform validate: empty output")
	}

	var out tfValidateJSON
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return "", fmt.Errorf("terraform validate: parse json: %w", err)
	}

	var buf strings.Builder
	if out.Valid {
		fmt.Fprintf(&buf, "terraform validate: OK (%d warning(s))\n", out.WarningCount)
	} else {
		fmt.Fprintf(&buf, "terraform validate: FAILED (%d error(s), %d warning(s))\n", out.ErrorCount, out.WarningCount)
	}
	for _, d := range out.Diagnostics {
		buf.WriteString(formatDiagLine(d.Severity, d.Range.Filename, d.Range.Start.Line, d.Summary, d.Detail))
		buf.WriteString("\n")
	}
	return buf.String(), nil
}

// formatTerraformValidateResult builds the "=== terraform validate ==="
// section body. It tries to parse out as terraform validate -json output
// first — this succeeds regardless of the command's exit code, which
// matters because terraform validate exits 1 whenever it finds errors, and
// runErr would otherwise cause real diagnostics to be discarded in favor of
// a generic error message (the bug this replaces). Only when out isn't
// parseable JSON does runErr get surfaced, e.g. because the terraform
// binary isn't installed at all (out is empty, runErr is a LookPath error).
func formatTerraformValidateResult(out string, runErr error) string {
	if summary, err := parseTerraformValidateJSON([]byte(out)); err == nil {
		return summary
	}
	if runErr != nil {
		return "terraform not found or error: " + runErr.Error() + "\n" + out
	}
	if strings.TrimSpace(out) == "" {
		return "terraform validate: no output."
	}
	return out
}

// --- tflint --format=json parsing ---
//
// Real shape observed by running `tflint --format=json` (TFLint v0.61.0)
// against the same broken fixture (lint warnings — no config errors):
//
//	{"issues":[{"rule":{"name":"terraform_required_version","severity":"warning", ...},
//	            "message":"terraform \"required_version\" attribute is required",
//	            "range":{"filename":"main.tf","start":{"line":1,"column":1},"end":{...}},
//	            "callers":[],"fixable":false,"fixed":false}],
//	 "errors":[]}
//
// ...and against a syntactically-broken fixture (unterminated string), which
// populates "errors" (tflint's own parse failures) rather than "issues"
// (lint rule violations found on config that did parse):
//
//	{"issues":[],
//	 "errors":[{"summary":"Invalid multi-line string",
//	            "message":"Quoted strings may not be split over multiple lines. ...",
//	            "severity":"error",
//	            "range":{"filename":"main.tf","start":{"line":2,"column":20},"end":{...}}}]}
//
// This confirms the brief's sketch of a top-level {"issues": [...], "errors":
// [...]} shape; the two arrays have different item shapes (issues nest a
// "rule" object, errors carry their own "summary" instead), so they're
// decoded as separate types below.

type tflintRange struct {
	Filename string `json:"filename"`
	Start    struct {
		Line int `json:"line"`
	} `json:"start"`
}

type tflintIssue struct {
	Rule struct {
		Name     string `json:"name"`
		Severity string `json:"severity"`
	} `json:"rule"`
	Message string      `json:"message"`
	Range   tflintRange `json:"range"`
}

type tflintErrorItem struct {
	Summary  string      `json:"summary"`
	Message  string      `json:"message"`
	Severity string      `json:"severity"`
	Range    tflintRange `json:"range"`
}

type tflintJSON struct {
	Issues []tflintIssue     `json:"issues"`
	Errors []tflintErrorItem `json:"errors"`
}

// parseTflintJSON turns tflint --format=json's output into a compact,
// structured summary, one line per issue/error in the same
// "<severity>: <file>:<line>: <title> — <message>" style as
// parseTerraformValidateJSON, so the two tools read consistently in the
// combined validate_terraform output.
//
// Returns an error if data isn't tflint's JSON shape — the caller
// (formatTflintResult) falls back to raw text in that case.
func parseTflintJSON(data []byte) (string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("tflint: empty output")
	}

	var out tflintJSON
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return "", fmt.Errorf("tflint: parse json: %w", err)
	}

	var buf strings.Builder
	if len(out.Issues) == 0 && len(out.Errors) == 0 {
		buf.WriteString("tflint: OK (0 issues, 0 errors)\n")
		return buf.String(), nil
	}
	fmt.Fprintf(&buf, "tflint: %d issue(s), %d error(s)\n", len(out.Issues), len(out.Errors))
	for _, iss := range out.Issues {
		buf.WriteString(formatDiagLine(iss.Rule.Severity, iss.Range.Filename, iss.Range.Start.Line, iss.Rule.Name, iss.Message))
		buf.WriteString("\n")
	}
	for _, e := range out.Errors {
		title := e.Summary
		if title == "" {
			title = "tflint error"
		}
		buf.WriteString(formatDiagLine(e.Severity, e.Range.Filename, e.Range.Start.Line, title, e.Message))
		buf.WriteString("\n")
	}
	return buf.String(), nil
}

// formatTflintResult builds the "=== tflint ===" section body. Like
// formatTerraformValidateResult, it tries JSON parsing first regardless of
// runErr, because tflint exits non-zero (2) whenever it finds lint issues —
// treating that exit code as a hard failure would discard real, parseable
// findings (the bug this replaces). runErr is only surfaced when out isn't
// parseable JSON, e.g. tflint isn't installed.
func formatTflintResult(out string, runErr error) string {
	if summary, err := parseTflintJSON([]byte(out)); err == nil {
		return summary
	}
	if runErr != nil {
		return "tflint not found or error: " + runErr.Error() + "\n" + out
	}
	if strings.TrimSpace(out) == "" {
		return "tflint: no output."
	}
	return out
}

// formatDiagLine renders one diagnostic/issue/error as a single compact
// line: "<severity>: <file>:<line>: <title> — <detail>". filename/line are
// omitted when absent (some diagnostics aren't tied to a source location),
// and the " — <detail>" suffix is omitted when detail is empty so titles
// that are already the full message don't get a trailing " — ".
func formatDiagLine(severity, filename string, line int, title, detail string) string {
	sev := severity
	if sev == "" {
		sev = "error"
	}

	var loc string
	switch {
	case filename != "" && line > 0:
		loc = fmt.Sprintf("%s:%d: ", filename, line)
	case filename != "":
		loc = filename + ": "
	}

	if detail != "" && detail != title {
		return fmt.Sprintf("%s: %s%s — %s", sev, loc, title, detail)
	}
	return fmt.Sprintf("%s: %s%s", sev, loc, title)
}
