import { useEffect, useRef, useState } from "react";
import type { OutputLine } from "../types";
import type { RunState } from "../hooks/useTaskRunner";

const SPINNER_FRAMES = ["⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"];

interface Props {
  output: OutputLine[];
  state: RunState;
  prUrl: string | null;
  pendingQuestion?: string | null;
  onAnswer?: (answer: string) => void;
  onRetry?: () => void;
  onCancel?: () => void;
}

// ── Display item types built from raw OutputLine[] ────────────────────────────
type DisplayItem =
  | { kind: "text";   id: string; content: string }
  | { kind: "tool";   id: string; toolName: string; label: string; done: boolean; output: string }
  | { kind: "error";  id: string; content: string }
  | { kind: "status"; id: string; content: string };

// Walk the output array in order, pairing tool_start/tool_end into single items.
function buildDisplayItems(lines: OutputLine[]): DisplayItem[] {
  const result: DisplayItem[] = [];

  for (const line of lines) {
    if (line.kind === "text") {
      result.push({ kind: "text", id: line.id, content: line.content });
    } else if (line.kind === "tool_start") {
      result.push({
        kind: "tool",
        id: line.id,
        toolName: line.toolName ?? "",
        label: friendlyLabel(line.toolName ?? ""),
        done: false,
        output: "",
      });
    } else if (line.kind === "tool_end") {
      // Find the last unpaired tool with this name and mark it done.
      const toolName = line.toolName ?? "";
      for (let i = result.length - 1; i >= 0; i--) {
        const item = result[i];
        if (item.kind === "tool" && item.toolName === toolName && !item.done) {
          item.done = true;
          item.output = line.content;
          break;
        }
      }
    } else if (line.kind === "error") {
      result.push({ kind: "error", id: line.id, content: line.content });
    } else if (line.kind === "status") {
      result.push({ kind: "status", id: line.id, content: line.content });
    }
  }

  return result;
}

export function OutputPanel({ output, state, prUrl, pendingQuestion, onAnswer, onRetry, onCancel }: Props) {
  const bottomRef = useRef<HTMLDivElement>(null);
  const [spinnerFrame, setSpinnerFrame] = useState(0);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [answerDraft, setAnswerDraft] = useState("");

  useEffect(() => {
    if (state !== "streaming") return;
    const id = setInterval(() => setSpinnerFrame((f) => (f + 1) % SPINNER_FRAMES.length), 100);
    return () => clearInterval(id);
  }, [state]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [output]);

  const toggleExpand = (id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  };

  const submitAnswer = () => {
    const a = answerDraft.trim();
    if (!a || !onAnswer) return;
    setAnswerDraft("");
    onAnswer(a);
  };

  const items = buildDisplayItems(output);
  const lastItem = items[items.length - 1];
  // ID of the currently-running tool (last tool_start with no matching tool_end yet).
  const activeToolId = [...items].reverse().find((i) => i.kind === "tool" && !i.done)?.id ?? null;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16, animation: "slideUp .22s ease" }}>

      {/* ── Status bar ───────────────────────────────────────────────────────── */}
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
          <span style={{ fontSize: "var(--text-xs)", fontWeight: 600, letterSpacing: ".08em", textTransform: "uppercase", color: "var(--text-3)" }}>
            Output
          </span>
          {(state === "streaming" || state === "submitting") && <span className="chip chip-running">Running</span>}
          {state === "waiting" && <span className="chip chip-waiting">Waiting for you</span>}
          {state === "done"    && <span className="chip chip-done">Done</span>}
          {state === "error"   && <span className="chip chip-failed">Failed</span>}
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          {(state === "streaming" || state === "submitting" || state === "waiting") && onCancel && (
            <button className="btn btn-sm" style={{ color: "var(--red)", borderColor: "#f5c6c2" }} onClick={onCancel}>
              ⏹ Stop
            </button>
          )}
          {(state === "done" || state === "error") && onRetry && (
            <button className="btn btn-sm" onClick={onRetry}>↺ Retry</button>
          )}
        </div>
      </div>

      {/* ── Chronological stream ──────────────────────────────────────────────── */}
      {items.length > 0 && (
        <div className="card output-stream">
          {items.map((item) => {
            // ── Text block ──────────────────────────────────────────────────
            if (item.kind === "text") {
              return (
                <div key={item.id} className="stream-text">
                  <TextOutput content={item.content} />
                  {state === "streaming" && item === lastItem && (
                    <span className="stream-cursor" />
                  )}
                </div>
              );
            }

            // ── Tool block ──────────────────────────────────────────────────
            if (item.kind === "tool") {
              const isActive   = item.id === activeToolId && state === "streaming";
              const isExpanded = expanded.has(item.id);
              const hasOutput  = item.done && item.output.trim().length > 0;

              return (
                <div key={item.id} className="tool-block">
                  <button
                    className="tool-block-btn"
                    onClick={hasOutput ? () => toggleExpand(item.id) : undefined}
                    style={{ cursor: hasOutput ? "pointer" : "default" }}
                    title={hasOutput ? (isExpanded ? "Collapse output" : "Expand output") : undefined}
                  >
                    {/* Status icon */}
                    <span className="tool-block-icon">
                      {isActive ? (
                        <span style={{ fontFamily: "system-ui", fontSize: 14, color: "var(--accent)", lineHeight: 1 }}>
                          {SPINNER_FRAMES[spinnerFrame]}
                        </span>
                      ) : item.done ? (
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="var(--green)" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
                          <polyline points="20 6 9 17 4 12" />
                        </svg>
                      ) : (
                        <div style={{ width: 8, height: 8, borderRadius: "50%", background: "var(--border-strong)" }} />
                      )}
                    </span>

                    {/* Label */}
                    <span className="tool-block-label">
                      {item.label}
                      {isActive && (
                        <span style={{ fontSize: "var(--text-xs)", color: "var(--accent)", marginLeft: 8, fontStyle: "italic" }}>
                          running…
                        </span>
                      )}
                    </span>

                    {/* Expand chevron — only when there's output to show */}
                    {hasOutput && (
                      <svg
                        width="11" height="11" viewBox="0 0 24 24" fill="none"
                        stroke="var(--text-3)" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"
                        style={{ marginLeft: "auto", flexShrink: 0, transition: "transform .15s", transform: isExpanded ? "rotate(90deg)" : "rotate(0deg)" }}
                      >
                        <polyline points="9 18 15 12 9 6" />
                      </svg>
                    )}
                  </button>

                  {/* Expanded tool output */}
                  {isExpanded && hasOutput && (
                    <div className="tool-block-output">
                      <pre>{item.output.trim()}</pre>
                    </div>
                  )}
                </div>
              );
            }

            // ── Error ───────────────────────────────────────────────────────
            if (item.kind === "error") {
              return <div key={item.id} className="error-box stream-error">{item.content}</div>;
            }

            // ── Status annotation (e.g. "You: <answer>") ────────────────────
            if (item.kind === "status") {
              return (
                <div key={item.id} className="stream-status">{item.content}</div>
              );
            }

            return null;
          })}
        </div>
      )}

      {/* ── PR banner ─────────────────────────────────────────────────────────── */}
      {prUrl && (
        <div className="pr-card">
          <div>
            <div className="pr-card-title"><span style={{ marginRight: 6 }}>✓</span>Pull request opened</div>
            <div className="pr-card-url">{prUrl}</div>
          </div>
          <a href={prUrl} target="_blank" rel="noopener noreferrer" className="btn btn-sm">View PR ↗</a>
        </div>
      )}

      {/* ── Waiting-for-input — sticky prominent callout ──────────────────────── */}
      {state === "waiting" && pendingQuestion && (
        <div className="waiting-callout">
          <div className="waiting-callout-header">
            <span className="waiting-callout-icon">?</span>
            <span style={{ fontSize: "var(--text-sm)", fontWeight: 600, color: "var(--text)" }}>
              Agent needs clarification
            </span>
          </div>
          <p className="waiting-callout-question">{pendingQuestion}</p>
          <div style={{ display: "flex", gap: 8 }}>
            <textarea
              className="form-input"
              rows={2}
              placeholder="Type your answer…"
              value={answerDraft}
              onChange={(e) => setAnswerDraft(e.target.value)}
              onKeyDown={(e) => {
                if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
                  e.preventDefault();
                  submitAnswer();
                }
              }}
              style={{ flex: 1, resize: "none" }}
              autoFocus
            />
            <button
              className="btn btn-primary"
              onClick={submitAnswer}
              disabled={!answerDraft.trim()}
              style={{ alignSelf: "flex-end", whiteSpace: "nowrap" }}
            >
              Send →
            </button>
          </div>
          <div style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", marginTop: 6 }}>
            ⌘ + Enter to send
          </div>
        </div>
      )}

      <div ref={bottomRef} />
    </div>
  );
}

// ── Render text, detecting fenced code blocks ─────────────────────────────────
function TextOutput({ content }: { content: string }) {
  const parts = content.split(/(```[\w]*\n[\s\S]*?```)/g);
  return (
    <>
      {parts.map((part, i) => {
        const m = part.match(/^```([\w]*)\n([\s\S]*?)```$/);
        if (m) {
          return <InlineCodeBlock key={i} lang={m[1] || "text"} code={m[2]} />;
        }
        if (!part) return null;
        return (
          <span key={i} style={{
            display: "block",
            fontFamily: "var(--font-mono)",
            fontSize: "var(--text-code)",
            color: "var(--text-2)",
            lineHeight: 1.75,
            whiteSpace: "pre-wrap",
            wordBreak: "break-word",
            marginBottom: 2,
          }}>
            {part}
          </span>
        );
      })}
    </>
  );
}

function InlineCodeBlock({ lang, code }: { lang: string; code: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(code).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  };
  return (
    <div style={{ margin: "10px 0", border: "1px solid var(--border)", borderRadius: 6, overflow: "hidden" }}>
      <div style={{
        display: "flex", alignItems: "center", justifyContent: "space-between",
        background: "var(--surface-2)", padding: "6px 12px", borderBottom: "1px solid var(--border)",
      }}>
        <span style={{ fontFamily: "var(--font-mono)", fontSize: 10, color: "var(--text-3)", textTransform: "uppercase", letterSpacing: ".06em", fontWeight: 500 }}>
          {lang}
        </span>
        <button className={`copy-btn${copied ? " copied" : ""}`} onClick={copy} style={{ fontSize: 10, padding: "2px 7px" }}>
          {copied ? "✓" : "Copy"}
        </button>
      </div>
      <pre style={{
        margin: 0, padding: "12px 14px",
        fontFamily: "var(--font-mono)", fontSize: "var(--text-code)",
        color: "var(--text)", lineHeight: 1.7,
        overflowX: "auto", whiteSpace: "pre-wrap", wordBreak: "break-word",
        background: "var(--surface)",
      }}>
        {code}
      </pre>
    </div>
  );
}

// ── Map raw tool names to friendly labels ─────────────────────────────────────
function friendlyLabel(toolName: string): string {
  const n = toolName.toLowerCase();
  if (n === "repo_scan")                               return "Comprehending";
  if (n === "generate_terraform")                      return "Generating code";
  if (n === "validate_terraform")                      return "Validating";
  if (n === "securityscan")                            return "Security scanning";
  if (n === "createpr")                                return "Opening PR";
  if (n === "clarifier")                               return "Clarifying";
  if (n === "detect_drift")                            return "Detecting drift";
  if (n === "jira_fetch")                              return "Fetching Jira";
  if (n === "task")                                    return "Task created";
  if (n === "agent")                                   return "Orchestrating";
  if (n === "ask_user")                                return "Asking you";
  if (n === "read" || n === "ls")                      return "Reading files";
  if (n === "glob" || n === "grep")                    return "Searching codebase";
  if (n === "bash")                                    return "Running commands";
  if (n === "write")                                   return "Writing code";
  if (n === "edit")                                    return "Editing code";
  if (n === "web_fetch" || n === "web_search")         return "Researching";
  return "Working";
}
