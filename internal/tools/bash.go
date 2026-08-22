package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// BashTool executes shell commands with safety guardrails.
type BashTool struct{}

func (t *BashTool) Name() string                         { return "bash" }
func (t *BashTool) IsReadOnly() bool                     { return false }
func (t *BashTool) IsDestructive(_ json.RawMessage) bool { return true }

func (t *BashTool) Description() string {
	return "Execute a shell command. Timeout defaults to 120 seconds. Use for running scripts, installing packages, checking git status, etc."
}

func (t *BashTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The shell command to execute"
			},
			"timeout": {
				"type": "integer",
				"description": "Timeout in seconds (default 120, max 600)"
			}
		},
		"required": ["command"]
	}`)
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// blockedPatterns are literal shell substrings that must never be executed.
//
// NOTE: this is a defense-in-depth blocklist, not a sandbox. A determined
// caller can still construct a command that slips past pattern matching
// (that's exactly why full command isolation is tracked as separate,
// larger future work). The goal here is to raise the bar against obviously
// dangerous top-level commands while keeping legitimate terraform/git/go/npm
// tool invocations working, not to achieve completeness.
// "rm -rf /" is intentionally not a literal entry here: a plain substring
// match on it also flags perfectly ordinary commands like "rm -rf /tmp/foo",
// which this tool needs to run all the time. It's instead covered precisely
// (root, wildcard-root, home, $HOME, bare ".", but not subpaths) by the
// rm-recursive-delete regex in blockedRegexPatterns below.
var blockedPatterns = []string{
	":(){ :|:& };:",
	"mkfs",
	"dd if=/dev/zero",
}

// blockedRegexPatterns cover dangerous command shapes that a literal
// substring can't express (flag ordering/spacing, piping, redirection,
// argument position, etc). Each entry pairs a compiled pattern with the
// human-readable reason surfaced in the error.
var blockedRegexPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	// Privilege escalation: sudo/doas/su used as a command (not merely a
	// substring inside some other word), at the start of the command or
	// after a shell separator (;, &, |, &&, ||).
	{
		regexp.MustCompile(`(?:^|[;&|]\s*)(sudo|doas|su)(\s|$)`),
		"privilege escalation command",
	},
	// Piping fetched remote content directly into an interpreter, e.g.
	// `curl ... | sh`, `wget -qO- ... | bash`, `... | sudo python3`.
	{
		regexp.MustCompile(`(?i)\b(curl|wget)\b[^|;&\n]*\|\s*(sudo\s+)?(sh|bash|zsh|ksh|dash|python3?|perl|ruby|node)\b`),
		"piping fetched remote content into an interpreter",
	},
	// Process-substitution equivalent: `bash <(curl ...)`.
	{
		regexp.MustCompile(`(?i)\b(sh|bash|zsh|ksh|dash|python3?)\s+<\(\s*(curl|wget)\b`),
		"process-substitution execution of fetched remote content",
	},
	// `eval` of fetched remote content, e.g. eval "$(curl ...)".
	{
		regexp.MustCompile(`(?i)\beval\b[^\n]*\b(curl|wget)\b`),
		"eval of fetched remote content",
	},
	// Fetch-then-execute as two chained commands on one line, e.g.
	// `curl -o /tmp/x.sh URL && bash /tmp/x.sh`.
	{
		regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n;&|]*\s(-o|-O|--output)\b[^\n;&|]*(&&|;)\s*(sudo\s+)?(sh|bash|zsh|ksh|dash|python3?|perl|ruby|node)\b`),
		"fetching and then executing a script in one command",
	},
	// Fetch-then-chmod+x as two chained commands, e.g.
	// `curl -o x.sh URL && chmod +x x.sh`.
	{
		regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n]*(-o|-O|--output)\b[^\n]*&&[^\n]*\bchmod\s+\+x\b`),
		"fetching a script and making it executable in one command",
	},
	// Recursive delete targeting the filesystem root, a wildcard root
	// expansion, home (~ or $HOME), or the bare current directory. Matches
	// `-r`/`-rf`/`-fr`/`-R`/`--recursive` in any flag ordering, but only
	// when the target is the bare dangerous path itself (e.g. `rm -rf /`),
	// not a subpath like `rm -rf /var/tmp` or `rm -rf ./build`.
	{
		regexp.MustCompile(`(?i)\brm\s+(?:-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)(?:\s+(?:-[a-zA-Z]+|--\w+))*\s+['"]?(?:/|/\*|~|\$HOME|\.)['"]?(?:\s|$|;|&|\|)`),
		"recursive delete of a filesystem root, home, or current directory",
	},
	// Raw writes to block devices.
	{
		regexp.MustCompile(`(?i)\bdd\b[^\n]*\bof=/dev/\S+`),
		"raw write to a block device via dd",
	},
	{
		regexp.MustCompile(`(?i)(?:>{1,2}|\|\s*tee\b(?:\s+-a)?)\s*['"]?/dev/(?:sd|hd|nvme|xvd)\w*`),
		"raw redirect into a block device",
	},
	// Writes to files that would grant persistence (cron, passwd/shadow,
	// sudoers, systemd units, SSH authorized_keys).
	{
		regexp.MustCompile(`(?i)(?:>{1,2}|\|\s*tee\b(?:\s+-a)?)\s*['"]?(?:/etc/cron\S*|/etc/passwd|/etc/shadow|/etc/sudoers\S*|/etc/systemd/system/\S*|~/\.ssh/authorized_keys|\$HOME/\.ssh/authorized_keys)`),
		"write to a persistence-sensitive system file",
	},
}

// forkBombPattern matches the classic `:(){ :|:& };:` fork bomb shape with
// an arbitrary (but consistent) identifier in place of `:`, e.g.
// `bomb(){ bomb|bomb& };bomb`. The four captured identifiers must all be
// identical for it to be treated as a fork bomb.
var forkBombPattern = regexp.MustCompile(`([a-zA-Z0-9_:]+)\s*\(\)\s*\{\s*([a-zA-Z0-9_:]+)\s*\|\s*([a-zA-Z0-9_:]+)\s*&?\s*\}\s*;\s*([a-zA-Z0-9_:]+)`)

// blockedReason returns a non-empty reason if command matches one of the
// safety blocklists, or "" if the command is allowed to run. It performs no
// side effects, so it's safe to call directly (e.g. from tests) without
// spawning a subprocess.
func blockedReason(command string) string {
	for _, blocked := range blockedPatterns {
		if strings.Contains(command, blocked) {
			return fmt.Sprintf("contains %q", blocked)
		}
	}
	for _, p := range blockedRegexPatterns {
		if p.re.MatchString(command) {
			return p.reason
		}
	}
	if m := forkBombPattern.FindStringSubmatch(command); m != nil {
		if m[1] == m[2] && m[2] == m[3] && m[3] == m[4] {
			return "fork bomb pattern"
		}
	}
	return ""
}

func (t *BashTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("bash: invalid input: %w", err)
	}
	if args.Command == "" {
		return "", fmt.Errorf("bash: command is required")
	}

	// Safety check.
	if reason := blockedReason(args.Command); reason != "" {
		return "", fmt.Errorf("bash: command blocked for safety: %s", reason)
	}

	timeout := args.Timeout
	if timeout <= 0 {
		timeout = 120
	}
	if timeout > 600 {
		timeout = 600
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := stdout.String()
	if stderr.Len() > 0 {
		if out != "" {
			out += "\n"
		}
		out += stderr.String()
	}

	// Strip ANSI codes.
	out = ansiEscape.ReplaceAllString(out, "")

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("bash: command timed out after %ds", timeout)
		}
		// Return output + error message (exit code errors are informative).
		if out == "" {
			out = err.Error()
		}
	}

	if len(out) > 100_000 {
		out = out[:100_000] + "\n... (truncated)"
	}
	return out, nil
}
