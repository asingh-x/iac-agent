import { useEffect, useRef, useState } from "react";
import { answerTask, cancelTask, getTask } from "../lib/api";
import type { TaskDetail as TTaskDetail } from "../types";

interface Props {
  taskId: string;
  onBack?: () => void;
}

export function TaskDetail({ taskId, onBack }: Props) {
  const [task, setTask] = useState<TTaskDetail | null>(null);
  const [error, setError] = useState("");
  const [answer, setAnswer] = useState("");
  const [sending, setSending] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const answerRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    setTask(null);
    setError("");
    getTask(taskId)
      .then(setTask)
      .catch(() => setError("Failed to load task."));
  }, [taskId]);

  if (error) {
    return <div className="error-box">{error}</div>;
  }

  if (!task) {
    return (
      <div className="card" style={{ padding: "20px 24px", display: "flex", alignItems: "center", gap: 10 }}>
        <div className="tl-dot pulse" />
        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>Loading task…</span>
      </div>
    );
  }

  const handleSendAnswer = async () => {
    if (!answer.trim() || sending) return;
    setSending(true);
    try {
      await answerTask(taskId, answer.trim());
      setAnswer("");
      // Refresh task to clear pending_question
      getTask(taskId).then(setTask).catch(() => {});
    } finally {
      setSending(false);
    }
  };

  const cleanInput = task.input_text.replace(/^\[.*?\]\s*/g, "");
  const durationSec = task.started_at && task.completed_at
    ? Math.round((new Date(task.completed_at).getTime() - new Date(task.started_at).getTime()) / 1000)
    : null;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16, animation: "slideUp .2s ease" }}>

      {/* Back button */}
      {onBack && (
        <div>
          <button className="btn btn-sm" onClick={onBack}>← Back to Recent Tasks</button>
        </div>
      )}

      {/* Status header */}
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
        <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
          <span style={{ fontSize: "var(--text-xs)", fontWeight: 600, letterSpacing: ".08em", textTransform: "uppercase", color: "var(--text-3)" }}>
            Task
          </span>
          <span className={`status-badge ${task.status}`}>
            {task.status}
          </span>
          {(task.status === "waiting_for_input" || task.status === "running" || task.status === "queued") && (
            <button
              className="btn btn-sm"
              style={{ color: "var(--red)", borderColor: "#f5c6c2" }}
              disabled={cancelling}
              onClick={async () => {
                setCancelling(true);
                await cancelTask(taskId).catch(() => {});
                // Poll until status reflects the cancellation (server update is async).
                for (let i = 0; i < 5; i++) {
                  await new Promise((r) => setTimeout(r, 700));
                  const updated = await getTask(taskId).catch(() => null);
                  if (updated) {
                    setTask(updated);
                    if (updated.status !== "running" && updated.status !== "waiting_for_input" && updated.status !== "queued") break;
                  }
                }
                setCancelling(false);
              }}
            >
              {cancelling ? "Cancelling…" : "⏹ Cancel"}
            </button>
          )}
        </div>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", fontFamily: "var(--font-mono)" }}>
          {task.id}
        </span>
      </div>

      {/* Task input */}
      <div className="card" style={{ padding: "18px 22px" }}>
        <div style={{ fontSize: "var(--text-xs)", fontWeight: 600, textTransform: "uppercase", letterSpacing: ".08em", color: "var(--text-3)", marginBottom: 10 }}>
          Task description
        </div>
        <p style={{ fontSize: "var(--text-sm)", color: "var(--text-2)", lineHeight: 1.7, whiteSpace: "pre-wrap" }}>
          {cleanInput}
        </p>
      </div>

      {/* Meta row */}
      <div className="card" style={{ padding: "16px 22px" }}>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 16 }}>
          <MetaField label="Created" value={new Date(task.created_at).toLocaleString()} />
          {durationSec !== null && <MetaField label="Duration" value={humanizeDuration(durationSec)} />}
          {(task.input_tokens > 0 || task.output_tokens > 0) && (
            <MetaField
              label="Tokens"
              value={`${(task.input_tokens + task.output_tokens).toLocaleString()} total`}
            />
          )}
          {task.output_type && <MetaField label="Output type" value={task.output_type} />}
        </div>
      </div>

      {/* Cancelled notice */}
      {task.status === "cancelled" && (
        <div style={{ fontSize: "var(--text-sm)", color: "var(--amber)", background: "var(--amber-bg)", border: "1px solid #fde68a", borderRadius: "var(--radius-sm)", padding: "10px 14px" }}>
          Task was cancelled by user.
        </div>
      )}

      {/* Error */}
      {task.error_msg && task.status !== "cancelled" && (
        <div className="error-box">
          <strong>Error:</strong> {task.error_msg}
        </div>
      )}

      {/* Waiting for input — survives refresh */}
      {task.status === "waiting_for_input" && task.pending_question && (
        <div className="card" style={{ borderColor: "var(--border)", animation: "slideUp .2s ease" }}>
          <div style={{ marginBottom: 16 }}>
            <div style={{ fontSize: "var(--text-sm)", fontWeight: 600, color: "var(--text)", marginBottom: 4 }}>
              <span style={{ color: "var(--accent)", marginRight: 6 }}>?</span>Agent needs clarification
            </div>
            <div style={{ fontSize: "var(--text-sm)", color: "var(--text-2)", whiteSpace: "pre-wrap", lineHeight: 1.6 }}>
              {task.pending_question}
            </div>
          </div>
          <div style={{ display: "flex", gap: 8 }}>
            <textarea
              ref={answerRef}
              className="form-input"
              rows={2}
              placeholder="Type your answer…"
              value={answer}
              onChange={(e) => setAnswer(e.target.value)}
              onKeyDown={(e) => { if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); handleSendAnswer(); } }}
              style={{ flex: 1, resize: "none" }}
              autoFocus
            />
            <button
              className="btn btn-primary"
              onClick={handleSendAnswer}
              disabled={sending || !answer.trim()}
              style={{ alignSelf: "flex-end", whiteSpace: "nowrap" }}
            >
              {sending ? "Sending…" : "Send →"}
            </button>
          </div>
          <div style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", marginTop: 6 }}>
            ⌘ + Enter to send
          </div>
        </div>
      )}

      {/* PR link */}
      {task.pr_url && (
        <div className="pr-card">
          <div>
            <div className="pr-card-title">
              <span style={{ marginRight: 6 }}>✓</span> Pull request opened
            </div>
            <div className="pr-card-url">{task.pr_url}</div>
          </div>
          <a href={task.pr_url} target="_blank" rel="noopener noreferrer" className="btn btn-sm">
            View PR ↗
          </a>
        </div>
      )}

      {/* Generated output */}
      {task.output && (
        <div className="card" style={{ padding: 0, overflow: "hidden" }}>
          <div style={{
            padding: "10px 16px", borderBottom: "1px solid var(--border)",
            fontSize: "var(--text-xs)", fontWeight: 600, textTransform: "uppercase",
            letterSpacing: ".08em", color: "var(--text-3)",
          }}>
            Generated output
          </div>
          <pre style={{
            margin: 0, padding: "16px", overflowX: "auto",
            fontFamily: "var(--font-mono)", fontSize: "var(--text-xs)",
            color: "var(--text-2)", lineHeight: 1.6, whiteSpace: "pre-wrap",
            wordBreak: "break-word",
          }}>
            {task.output}
          </pre>
        </div>
      )}

    </div>
  );
}

function humanizeDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return s > 0 ? `${m}m ${s}s` : `${m}m`;
}

function MetaField({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", fontWeight: 600, textTransform: "uppercase", letterSpacing: ".06em", marginBottom: 3 }}>
        {label}
      </div>
      <div style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>{value}</div>
    </div>
  );
}
