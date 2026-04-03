import { useEffect, useState } from "react";
import { cancelTask, listTasks } from "../lib/api";
import type { TaskDetail } from "../types";

interface Props {
  onSelectTask: (task: TaskDetail) => void;
  refreshTrigger: number;
}

const PAGE_SIZE_OPTIONS = [10, 25, 50];

export function HistoryPage({ onSelectTask, refreshTrigger }: Props) {
  const [tasks, setTasks] = useState<TaskDetail[]>([]);
  const [loading, setLoading] = useState(true);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(10);
  const [hoveredId, setHoveredId] = useState<string | null>(null);

  useEffect(() => {
    setLoading(true);
    listTasks()
      .then((t) => setTasks(t ?? []))
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [refreshTrigger]);

  const totalPages = Math.max(1, Math.ceil(tasks.length / pageSize));
  const safePage = Math.min(page, totalPages);
  const start = (safePage - 1) * pageSize;
  const paginated = tasks.slice(start, start + pageSize);

  const toggleExpand = (id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  };

  return (
    <div style={{ animation: "slideUp .2s ease" }}>
      {/* Header */}
      <div style={{ marginBottom: 24 }}>
        <div style={{ fontSize: "var(--text-lg)", fontWeight: 700, color: "var(--text)", letterSpacing: "-.02em" }}>
          Recent Tasks
        </div>
        <div style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", marginTop: 3 }}>
          All tasks submitted to tf-agent
        </div>
      </div>

      {/* Table card */}
      <div className="card" style={{ padding: 0, overflow: "hidden" }}>
        {loading ? (
          <div style={{ padding: "32px 24px", display: "flex", alignItems: "center", gap: 10 }}>
            <div className="tl-dot pulse" />
            <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>Loading…</span>
          </div>
        ) : tasks.length === 0 ? (
          <div style={{ padding: "48px 24px", textAlign: "center" }}>
            <div style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>No tasks yet.</div>
          </div>
        ) : (
          <>
            <table style={{ width: "100%", borderCollapse: "collapse" }}>
              <thead>
                <tr style={{ background: "var(--surface)", borderBottom: "1px solid var(--border)" }}>
                  <Th style={{ width: 28 }} />
                  <Th>Status</Th>
                  <Th>Task</Th>
                  <Th>Created</Th>
                  <Th>Duration</Th>
                  <Th>Tokens</Th>
                  <Th>PR</Th>
                </tr>
              </thead>
              <tbody>
                {paginated.map((task) => {
                  const isExpanded = expanded.has(task.id);
                  const isHovered  = hoveredId === task.id;
                  const label      = task.input_text.replace(/^\[.*?\]\s*/g, "");
                  const duration   = durationSec(task.started_at, task.completed_at);
                  const tokens     = (task.input_tokens ?? 0) + (task.output_tokens ?? 0);
                  const isJira     = /^\[JIRA:/i.test(task.input_text);

                  return (
                    <>
                      <tr
                        key={task.id}
                        onClick={() => toggleExpand(task.id)}
                        onMouseEnter={() => setHoveredId(task.id)}
                        onMouseLeave={() => setHoveredId(null)}
                        style={{
                          borderBottom: isExpanded ? "none" : "1px solid var(--border)",
                          cursor: "pointer",
                          background: isHovered && !isExpanded ? "var(--surface-2)" : "var(--surface)",
                          transition: "background .1s",
                        }}
                      >
                        <Td>
                          <svg
                            width="12" height="12" viewBox="0 0 24 24" fill="none"
                            stroke="var(--text-3)" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round"
                            style={{ transition: "transform .15s", transform: isExpanded ? "rotate(90deg)" : "rotate(0deg)", display: "block" }}
                          >
                            <polyline points="9 18 15 12 9 6" />
                          </svg>
                        </Td>
                        <Td>
                          <span className={`status-badge ${task.status}`}>{task.status}</span>
                        </Td>
                        <Td>
                          <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
                            {isJira && <span className="input-type-badge jira">Jira</span>}
                            <span style={{ fontSize: "var(--text-sm)", color: "var(--text)", fontWeight: 500 }}>
                              {label.slice(0, 64)}{label.length > 64 ? "…" : ""}
                            </span>
                          </div>
                        </Td>
                        <Td muted>{fmt(task.created_at)}</Td>
                        <Td muted>{duration !== null ? humanizeDuration(duration) : "—"}</Td>
                        <Td muted>{tokens > 0 ? tokens.toLocaleString() : "—"}</Td>
                        <Td>
                          {task.pr_url ? (
                            <a
                              href={task.pr_url}
                              target="_blank"
                              rel="noopener noreferrer"
                              onClick={(e) => e.stopPropagation()}
                              className="pr-chip"
                            >
                              <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
                                <circle cx="18" cy="18" r="3" /><circle cx="6" cy="6" r="3" /><path d="M13 6h3a2 2 0 0 1 2 2v7" /><line x1="6" y1="9" x2="6" y2="21" />
                              </svg>
                              PR
                            </a>
                          ) : "—"}
                        </Td>
                      </tr>

                      {/* Expanded detail row */}
                      {isExpanded && (
                        <tr key={`${task.id}-exp`} style={{ borderBottom: "1px solid var(--border)" }}>
                          <td />
                          <td colSpan={6} style={{ padding: "12px 16px 20px", background: "var(--surface)" }}>
                            <ExpandedRow task={task} onOpen={() => onSelectTask(task)} onRefresh={() => listTasks().then((t) => setTasks(t ?? [])).catch(() => {})} />
                          </td>
                        </tr>
                      )}
                    </>
                  );
                })}
              </tbody>
            </table>

            {/* Pagination */}
            <div style={{
              display: "flex", alignItems: "center", justifyContent: "space-between",
              padding: "12px 20px", borderTop: "1px solid var(--border)",
              background: "var(--surface)",
            }}>
              <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                Showing {start + 1}–{Math.min(start + pageSize, tasks.length)} of {tasks.length}
              </span>

              <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                  Per page
                  <select
                    value={pageSize}
                    onChange={(e) => { setPageSize(Number(e.target.value)); setPage(1); }}
                    style={{
                      fontSize: "var(--text-xs)", color: "var(--text)", background: "var(--surface)",
                      border: "1px solid var(--border-strong)", borderRadius: 5, padding: "3px 6px",
                      cursor: "pointer", outline: "none",
                    }}
                  >
                    {PAGE_SIZE_OPTIONS.map((n) => <option key={n} value={n}>{n}</option>)}
                  </select>
                </label>

                <div style={{ display: "flex", gap: 4 }}>
                  <PageBtn disabled={safePage <= 1} onClick={() => setPage(safePage - 1)}>←</PageBtn>
                  <span style={{ fontSize: "var(--text-xs)", color: "var(--text-2)", padding: "4px 8px", lineHeight: 1.6 }}>
                    {safePage} / {totalPages}
                  </span>
                  <PageBtn disabled={safePage >= totalPages} onClick={() => setPage(safePage + 1)}>→</PageBtn>
                </div>
              </div>
            </div>
          </>
        )}
      </div>
    </div>
  );
}

// ── Expanded row detail ────────────────────────────────────────────────────────
function ExpandedRow({ task, onOpen, onRefresh }: { task: TaskDetail; onOpen: () => void; onRefresh: () => void }) {
  const clean = task.input_text.replace(/^\[.*?\]\s*/g, "");
  const [cancelling, setCancelling] = useState(false);

  const handleCancel = async () => {
    setCancelling(true);
    await cancelTask(task.id).catch(() => {});
    // Poll until the status leaves the active states (server update is async).
    const poll = async (attempts: number) => {
      await new Promise((r) => setTimeout(r, 600));
      onRefresh();
      if (attempts > 1) poll(attempts - 1);
    };
    poll(4);
    setCancelling(false);
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      <p style={{ fontSize: "var(--text-sm)", color: "var(--text-2)", lineHeight: 1.7, whiteSpace: "pre-wrap", maxWidth: 600 }}>
        {clean}
      </p>

      {task.error_msg && task.status !== "cancelled" && (
        <div className="error-box" style={{ fontSize: "var(--text-xs)", padding: "8px 12px" }}>
          {task.error_msg}
        </div>
      )}
      {task.status === "cancelled" && (
        <div style={{ fontSize: "var(--text-xs)", color: "var(--amber)", background: "var(--amber-bg)", border: "1px solid #fde68a", borderRadius: "var(--radius-sm)", padding: "8px 12px" }}>
          Task was cancelled by user.
        </div>
      )}

      <div style={{ display: "flex", alignItems: "center", gap: 16 }}>
        {task.pr_url && (
          <a
            href={task.pr_url}
            target="_blank"
            rel="noopener noreferrer"
            className="btn btn-sm"
            style={{ color: "var(--green)", borderColor: "#a7d9bc", background: "var(--green-bg)" }}
          >
            View PR ↗
          </a>
        )}
        {(task.status === "waiting_for_input" || task.status === "running" || task.status === "queued") && (
          <button
            className="btn btn-sm"
            style={{ color: "var(--red)", borderColor: "#f5c6c2" }}
            onClick={handleCancel}
            disabled={cancelling}
          >
            {cancelling ? "Cancelling…" : "⏹ Cancel"}
          </button>
        )}
        <button className="btn btn-sm" onClick={onOpen}>
          Full detail →
        </button>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", fontFamily: "var(--font-mono)" }}>
          {task.id.slice(0, 8)}
        </span>
      </div>
    </div>
  );
}

// ── Small helpers ─────────────────────────────────────────────────────────────
function Th({ children, style }: { children?: React.ReactNode; style?: React.CSSProperties }) {
  return (
    <th style={{
      padding: "9px 14px", textAlign: "left",
      fontSize: "var(--text-xs)", fontWeight: 600, color: "var(--text-3)",
      textTransform: "uppercase", letterSpacing: ".07em", whiteSpace: "nowrap",
      ...style,
    }}>
      {children}
    </th>
  );
}

function Td({ children, muted }: { children?: React.ReactNode; muted?: boolean }) {
  return (
    <td style={{
      padding: "10px 14px", fontSize: "var(--text-sm)",
      color: muted ? "var(--text-3)" : "var(--text-2)",
      whiteSpace: "nowrap", verticalAlign: "middle",
    }}>
      {children ?? "—"}
    </td>
  );
}

function PageBtn({ children, disabled, onClick }: { children: React.ReactNode; disabled: boolean; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      style={{
        width: 28, height: 28, display: "flex", alignItems: "center", justifyContent: "center",
        background: "var(--surface)", border: "1px solid var(--border-strong)", borderRadius: 6,
        fontSize: "var(--text-sm)", color: disabled ? "var(--text-3)" : "var(--text-2)",
        cursor: disabled ? "not-allowed" : "pointer", opacity: disabled ? 0.5 : 1,
      }}
    >
      {children}
    </button>
  );
}

function fmt(iso: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) + " " +
    d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

function durationSec(start?: string, end?: string): number | null {
  if (!start || !end) return null;
  return Math.round((new Date(end).getTime() - new Date(start).getTime()) / 1000);
}

function humanizeDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return s > 0 ? `${m}m ${s}s` : `${m}m`;
}
