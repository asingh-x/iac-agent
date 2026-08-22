import { useEffect, useState } from "react";
import {
  listAdminUsers, createAdminUser, updateAdminUser,
  deleteAdminUser, setAdminUserActive,
  revokeAdminUserToken, regenerateAdminUserToken,
} from "../lib/api";
import type { AdminUser } from "../types";

type Modal =
  | { type: "edit"; user: AdminUser }
  | { type: "token"; username: string; token: string }
  | { type: "create" }
  | null;

export function AdminPage() {
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [loading, setLoading] = useState(true);
  const [modal, setModal] = useState<Modal>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const reload = () => {
    setLoading(true);
    listAdminUsers().then(setUsers).catch(() => {}).finally(() => setLoading(false));
  };

  useEffect(() => { reload(); }, []);

  const busy = (id: string, fn: () => Promise<void>) => async () => {
    setBusyId(id);
    setError(null);
    try { await fn(); reload(); }
    catch (e) { setError(e instanceof Error ? e.message : "Error"); }
    finally { setBusyId(null); }
  };

  const handleDelete = (u: AdminUser) => busy(u.id + "-del", async () => {
    if (!confirm(`Permanently delete ${u.username}? This cannot be undone.`)) return;
    await deleteAdminUser(u.id);
  })();

  const handleToggleActive = (u: AdminUser) => busy(u.id + "-active", async () => {
    if (!u.active && !confirm(`Reactivate ${u.username}?`)) return;
    if (u.active && !confirm(`Deactivate ${u.username}? They will lose access immediately.`)) return;
    await setAdminUserActive(u.id, !u.active);
  })();

  const handleRevoke = (u: AdminUser) => busy(u.id + "-revoke", async () => {
    if (!confirm(`Revoke ${u.username}'s token? They will lose access immediately.`)) return;
    await revokeAdminUserToken(u.id);
  })();

  const handleRegenerate = async (u: AdminUser) => {
    if (!confirm(`Generate a new token for ${u.username}? Their current token will stop working immediately.`)) return;
    setBusyId(u.id + "-regen");
    setError(null);
    try {
      const { token } = await regenerateAdminUserToken(u.id);
      setModal({ type: "token", username: u.username, token });
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Error");
    } finally {
      setBusyId(null);
    }
  };

  return (
    <div>
      <div style={{ marginBottom: 28 }}>
        <div style={{ fontSize: 22, fontWeight: 700, color: "var(--text)", letterSpacing: "-.02em" }}>
          User Management
        </div>
        <div style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", marginTop: 4 }}>
          Create and manage users who can access IAC Agent.
        </div>
      </div>

      {error && (
        <div className="error-box" style={{ marginBottom: 16 }}>{error}</div>
      )}

      {modal?.type === "create" && (
        <CreateUserForm
          onCreated={(token, username) => { setModal({ type: "token", username, token }); reload(); }}
          onCancel={() => setModal(null)}
        />
      )}

      {modal?.type === "edit" && (
        <EditUserForm
          user={modal.user}
          onSaved={() => { setModal(null); reload(); }}
          onCancel={() => setModal(null)}
        />
      )}

      {modal?.type === "token" && (
        <NewTokenBanner
          username={modal.username}
          token={modal.token}
          onDone={() => setModal(null)}
        />
      )}

      <div className="card" style={{ padding: 0, overflow: "hidden" }}>
        <div style={{
          display: "flex", alignItems: "center", justifyContent: "space-between",
          padding: "14px 20px", borderBottom: "1px solid var(--border)",
        }}>
          <span style={{ fontSize: "var(--text-sm)", fontWeight: 600, color: "var(--text)" }}>
            {loading ? "Loading…" : `${users.length} user${users.length !== 1 ? "s" : ""}`}
          </span>
          {!modal && (
            <button
              className="btn btn-primary"
              style={{ fontSize: "var(--text-sm)", padding: "6px 14px" }}
              onClick={() => setModal({ type: "create" })}
            >
              + New user
            </button>
          )}
        </div>

        {!loading && users.length === 0 ? (
          <div style={{ padding: "24px 20px", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>No users yet.</div>
        ) : (
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ borderBottom: "1px solid var(--border)" }}>
                {["User", "Role", "Status", "Created", "Actions"].map((h) => (
                  <th key={h} style={{
                    padding: "10px 20px", textAlign: h === "Actions" ? "right" : "left",
                    fontSize: "var(--text-xs)", fontWeight: 600, color: "var(--text-3)",
                    textTransform: "uppercase", letterSpacing: ".06em",
                  }}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id}
                  style={{ borderBottom: "1px solid var(--border)" }}
                  onMouseEnter={e => (e.currentTarget.style.background = "var(--surface-2, #f5f4f0)")}
                  onMouseLeave={e => (e.currentTarget.style.background = "transparent")}
                >
                  {/* User */}
                  <td style={{ padding: "12px 20px" }}>
                    <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                      <div style={{
                        width: 28, height: 28, borderRadius: "50%", flexShrink: 0,
                        background: u.active ? "var(--accent)" : "var(--border-strong)",
                        color: "#fff", display: "flex", alignItems: "center", justifyContent: "center",
                        fontSize: 12, fontWeight: 700,
                      }}>
                        {u.username[0]?.toUpperCase() ?? "U"}
                      </div>
                      <span style={{ fontSize: "var(--text-sm)", fontWeight: 500, color: "var(--text)" }}>
                        {u.username}
                      </span>
                    </div>
                  </td>
                  {/* Role */}
                  <td style={{ padding: "12px 20px" }}>
                    <span style={{
                      fontSize: 11, fontWeight: 600, padding: "2px 8px", borderRadius: "var(--radius-sm)",
                      background: u.role === "admin" ? "var(--accent-subtle)" : "var(--surface-2, #f5f4f0)",
                      color: u.role === "admin" ? "var(--accent)" : "var(--text-2)",
                      border: u.role === "admin" ? "1px solid #f0c4b0" : "1px solid var(--border)",
                    }}>
                      {u.role}
                    </span>
                  </td>
                  {/* Status */}
                  <td style={{ padding: "12px 20px" }}>
                    <span style={{
                      fontSize: 11, fontWeight: 600, padding: "2px 8px", borderRadius: "var(--radius-sm)",
                      background: u.active ? "var(--green-bg, #edf7f1)" : "var(--surface-2)",
                      color: u.active ? "var(--green, #2d7a47)" : "var(--text-3)",
                      border: u.active ? "1px solid #a7d9bc" : "1px solid var(--border)",
                    }}>
                      {u.active ? "Active" : "Inactive"}
                    </span>
                  </td>
                  {/* Created */}
                  <td style={{ padding: "12px 20px", fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                    {new Date(u.created_at).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" })}
                  </td>
                  {/* Actions */}
                  <td style={{ padding: "12px 20px" }}>
                    <div style={{ display: "flex", gap: 4, justifyContent: "flex-end", flexWrap: "wrap" }}>
                      <ActionBtn
                        label="Edit"
                        disabled={!!busyId}
                        onClick={() => setModal({ type: "edit", user: u })}
                      />
                      <ActionBtn
                        label={busyId === u.id + "-active" ? "…" : u.active ? "Deactivate" : "Activate"}
                        disabled={!!busyId}
                        onClick={() => handleToggleActive(u)}
                      />
                      <ActionBtn
                        label={busyId === u.id + "-revoke" ? "…" : "Revoke key"}
                        disabled={!!busyId || !u.active}
                        onClick={() => handleRevoke(u)}
                        danger
                      />
                      <ActionBtn
                        label={busyId === u.id + "-regen" ? "…" : "New key"}
                        disabled={!!busyId}
                        onClick={() => handleRegenerate(u)}
                      />
                      <ActionBtn
                        label={busyId === u.id + "-del" ? "…" : "Delete"}
                        disabled={!!busyId}
                        onClick={() => handleDelete(u)}
                        danger
                      />
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

function ActionBtn({ label, onClick, disabled, danger }: {
  label: string; onClick: () => void; disabled?: boolean; danger?: boolean;
}) {
  return (
    <button
      className="btn"
      onClick={onClick}
      disabled={disabled}
      style={{
        fontSize: 11, padding: "3px 9px",
        background: "transparent",
        border: `1px solid ${danger ? "var(--red, #c0392b)" : "var(--border)"}`,
        color: danger ? "var(--red, #c0392b)" : "var(--text-2)",
        opacity: disabled ? 0.45 : 1,
      }}
    >
      {label}
    </button>
  );
}

function EditUserForm({ user, onSaved, onCancel }: { user: AdminUser; onSaved: () => void; onCancel: () => void }) {
  const [username, setUsername] = useState(user.username);
  const [role, setRole] = useState<"admin" | "member">(user.role as "admin" | "member");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSave = async () => {
    if (!username.trim()) {
      setError("Username is required.");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await updateAdminUser(user.id, { username: username.trim(), role });
      onSaved();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to update user");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="card" style={{ marginBottom: 20, animation: "slideUp .15s ease" }}>
      <div style={{ fontSize: "var(--text-base)", fontWeight: 600, color: "var(--text)", marginBottom: 16 }}>
        Edit — {user.username}
      </div>
      <div style={{ display: "flex", gap: 12, marginBottom: 16 }}>
        <div className="form-row" style={{ flex: 1, marginBottom: 0 }}>
          <label className="form-label">Username</label>
          <input
            type="text" className="form-input" value={username} autoFocus
            onChange={(e) => setUsername(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") handleSave(); }}
          />
        </div>
        <div className="form-row" style={{ width: 140, flexShrink: 0, marginBottom: 0 }}>
          <label className="form-label">Role</label>
          <select className="form-input" value={role} onChange={(e) => setRole(e.target.value as "admin" | "member")} style={{ cursor: "pointer" }}>
            <option value="member">Member</option>
            <option value="admin">Admin</option>
          </select>
        </div>
      </div>
      {error && <div className="error-box" style={{ marginBottom: 12 }}>{error}</div>}
      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
          {saving ? "Saving…" : "Save changes"}
        </button>
        <button className="btn" onClick={onCancel} style={{ background: "transparent", border: "1px solid var(--border)", color: "var(--text-2)" }}>
          Cancel
        </button>
      </div>
    </div>
  );
}

function NewTokenBanner({ username, token, onDone }: { username: string; token: string; onDone: () => void }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(token).then(() => { setCopied(true); setTimeout(() => setCopied(false), 2000); });
  };
  return (
    <div className="card" style={{ marginBottom: 20, borderColor: "var(--green, #2d7a47)", animation: "slideUp .2s ease" }}>
      <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 14 }}>
        <svg width="16" height="16" fill="none" stroke="var(--green, #2d7a47)" strokeWidth="2" viewBox="0 0 24 24">
          <polyline points="20 6 9 17 4 12"/>
        </svg>
        <span style={{ fontSize: "var(--text-sm)", fontWeight: 600, color: "var(--text)" }}>
          API key for <strong>{username}</strong>
        </span>
      </div>
      <div style={{ fontSize: "var(--text-sm)", color: "var(--text-2)", marginBottom: 12 }}>
        Copy this key now — it will <strong>never be shown again</strong>.
      </div>
      <div style={{
        display: "flex", alignItems: "center", gap: 8,
        background: "var(--code-bg, #1a1915)", borderRadius: "var(--radius-sm)", padding: "10px 14px",
        fontFamily: "var(--font-mono)", fontSize: "var(--text-xs)", color: "var(--code-text)", wordBreak: "break-all",
      }}>
        <span style={{ flex: 1 }}>{token}</span>
        <button className={`copy-btn${copied ? " copied" : ""}`} onClick={copy} style={{ flexShrink: 0, fontSize: 11 }}>
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
      <div style={{ marginTop: 14 }}>
        <button className="btn btn-primary" onClick={onDone}>Done</button>
      </div>
    </div>
  );
}

function CreateUserForm({ onCreated, onCancel }: { onCreated: (token: string, username: string) => void; onCancel: () => void }) {
  const [username, setUsername] = useState("");
  const [role, setRole] = useState<"admin" | "member">("member");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleCreate = async () => {
    if (!username.trim()) {
      setError("Username is required.");
      return;
    }
    setSaving(true);
    setError(null);
    // Generate tfa- prefixed token client-side for creation flow
    const raw = Array.from(crypto.getRandomValues(new Uint8Array(20))).map((b) => b.toString(16).padStart(2, "0")).join("");
    const token = "tfa-" + raw;
    try {
      await createAdminUser({ username: username.trim(), token, role });
      onCreated(token, username.trim());
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to create user");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="card" style={{ marginBottom: 20, animation: "slideUp .15s ease" }}>
      <div style={{ fontSize: "var(--text-base)", fontWeight: 600, color: "var(--text)", marginBottom: 16 }}>New user</div>
      <div style={{ display: "flex", gap: 12, marginBottom: 16 }}>
        <div className="form-row" style={{ flex: 1, marginBottom: 0 }}>
          <label className="form-label">Username</label>
          <input
            type="text" className="form-input" placeholder="e.g. jane" value={username} autoFocus
            onChange={(e) => setUsername(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") handleCreate(); }}
          />
        </div>
        <div className="form-row" style={{ width: 140, flexShrink: 0, marginBottom: 0 }}>
          <label className="form-label">Role</label>
          <select className="form-input" value={role} onChange={(e) => setRole(e.target.value as "admin" | "member")} style={{ cursor: "pointer" }}>
            <option value="member">Member</option>
            <option value="admin">Admin</option>
          </select>
        </div>
      </div>
      {error && <div className="error-box" style={{ marginBottom: 12 }}>{error}</div>}
      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn btn-primary" onClick={handleCreate} disabled={saving}>
          {saving ? "Creating…" : "Create user"}
        </button>
        <button className="btn" onClick={onCancel} style={{ background: "transparent", border: "1px solid var(--border)", color: "var(--text-2)" }}>
          Cancel
        </button>
      </div>
    </div>
  );
}
