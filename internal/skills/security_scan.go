package skills

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tf-agent/tf-agent/internal/sandbox"
)

//go:embed prompts/security_scan.md
var securityScanPrompt string

// SecurityScanSkill runs checkov on a directory.
// Input JSON: {"path": string (optional, default CWD)}
type SecurityScanSkill struct {
	// executor routes checkov through a sandbox.Executor — sandboxed
	// execution is mandatory, so a nil executor (the zero value, and what
	// &SecurityScanSkill{} still gives you) makes Execute return an error
	// rather than running checkov on the host. The field stays settable
	// directly for tests that supply a fake executor.
	executor sandbox.Executor
}

// NewSecurityScanSkill builds a SecurityScanSkill. executor must be non-nil
// in production — passing nil is supported only so tests can construct a
// SecurityScanSkill and separately exercise the "no sandbox executor
// configured" error path.
func NewSecurityScanSkill(executor sandbox.Executor) *SecurityScanSkill {
	return &SecurityScanSkill{executor: executor}
}

func (s *SecurityScanSkill) Name() string                         { return "SecurityScan" }
func (s *SecurityScanSkill) IsReadOnly() bool                     { return true }
func (s *SecurityScanSkill) IsDestructive(_ json.RawMessage) bool { return false }
func (s *SecurityScanSkill) Prompt() string                       { return securityScanPrompt }

func (s *SecurityScanSkill) Description() string {
	return "Run checkov security scan on a directory. Returns a summary of passed/failed checks."
}

func (s *SecurityScanSkill) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Directory to scan (default: current working directory)"
			}
		}
	}`)
}

func (s *SecurityScanSkill) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if input != nil {
		if err := json.Unmarshal(input, &args); err != nil {
			return "", fmt.Errorf("SecurityScan: invalid input: %w", err)
		}
	}

	scanPath := args.Path
	if scanPath == "" {
		var err error
		scanPath, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("SecurityScan: getwd: %w", err)
		}
	}

	if s.executor == nil {
		return "", fmt.Errorf("SecurityScan: no sandbox executor configured")
	}
	return s.runSandboxed(ctx, scanPath)
}

// runSandboxed runs checkov through s.executor. scanPath is bind-mounted at
// /workspace by the executor (see sandbox.DockerExecutor.Run) and set as the
// container's working directory, so checkov is pointed at "." rather than
// the host-absolute scanPath.
func (s *SecurityScanSkill) runSandboxed(ctx context.Context, scanPath string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	stdout, stderr, err := s.executor.Run(runCtx, scanPath, "checkov", "-d", ".", "-o", "json", "--quiet")

	// checkov exits non-zero when failures are found — still parse the output.
	// Only stdout is ever parsed: checkov --quiet still emits a WARNI log line
	// plus a Python traceback on stderr when it can't reach
	// api0.prismacloud.io for its guidelines mapping, which is guaranteed to
	// happen under --network=none — that noise must never be fed to
	// parseCheckovOutput.
	if err != nil && stdout == "" {
		return "", fmt.Errorf("SecurityScan: checkov failed: %v — %s", err, stderr)
	}
	return parseCheckovOutput([]byte(stdout))
}

type checkovOutput struct {
	Results struct {
		PassedChecks []struct {
			CheckID   string `json:"check_id"`
			CheckType string `json:"resource"`
		} `json:"passed_checks"`
		FailedChecks []struct {
			CheckID   string `json:"check_id"`
			CheckType string `json:"resource"`
			CheckMeta struct {
				Name string `json:"name"`
			} `json:"check"`
			FilePath  string `json:"repo_file_path"`
			FileLines []int  `json:"file_line_range"`
		} `json:"failed_checks"`
	} `json:"results"`
	Summary struct {
		Passed int `json:"passed"`
		Failed int `json:"failed"`
	} `json:"summary"`
}

func parseCheckovOutput(data []byte) (string, error) {
	// checkov may return a JSON array if multiple frameworks are run.
	rawData := bytes.TrimSpace(data)
	if len(rawData) == 0 {
		return "Security Scan: no output from checkov.", nil
	}

	var out checkovOutput
	// Try as a single object first.
	if rawData[0] == '[' {
		// It's an array — take the first element.
		var arr []json.RawMessage
		if err := json.Unmarshal(rawData, &arr); err != nil {
			return "", fmt.Errorf("SecurityScan: parse checkov output: %w", err)
		}
		if len(arr) == 0 {
			return "Security Scan: no results.", nil
		}
		if err := json.Unmarshal(arr[0], &out); err != nil {
			return "", fmt.Errorf("SecurityScan: parse checkov result: %w", err)
		}
	} else {
		if err := json.Unmarshal(rawData, &out); err != nil {
			return "", fmt.Errorf("SecurityScan: parse checkov output: %w", err)
		}
	}

	var buf strings.Builder
	fmt.Fprintf(&buf, "Security Scan: %d passed, %d failed\n", out.Summary.Passed, out.Summary.Failed)

	if len(out.Results.FailedChecks) > 0 {
		buf.WriteString("\nFAILED:\n")
		for _, fc := range out.Results.FailedChecks {
			lineInfo := ""
			if len(fc.FileLines) >= 1 {
				lineInfo = fmt.Sprintf(" (%s line %d)", fc.FilePath, fc.FileLines[0])
			} else if fc.FilePath != "" {
				lineInfo = fmt.Sprintf(" (%s)", fc.FilePath)
			}
			fmt.Fprintf(&buf, "- %s: %s%s\n", fc.CheckID, fc.CheckMeta.Name, lineInfo)
		}
	}

	return buf.String(), nil
}
