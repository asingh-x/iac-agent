import { useEffect, useState } from "react";
import { getSettings, saveSettings, updateMe } from "../lib/api";
import { AdminPage } from "./AdminPage";
import type { UserInfo, UserSettings } from "../types";

type Section = "profile" | "github" | "jira" | "users";

interface Props {
  userInfo: UserInfo | null;
  onUserUpdate: (info: UserInfo) => void;
}

const SS_SECTION = "tf_settings_section";

export function SettingsPage({ userInfo, onUserUpdate }: Props) {
  const [section, setSection] = useState<Section>(
    () => (sessionStorage.getItem(SS_SECTION) as Section) ?? "profile"
  );
  const [settings, setSettings] = useState<UserSettings | null>(null);

  const navSection = (s: Section) => {
    setSection(s);
    sessionStorage.setItem(SS_SECTION, s);
  };

  useEffect(() => {
    getSettings().then(setSettings).catch(() => {});
  }, []);

  const reload = () => getSettings().then(setSettings).catch(() => {});

  return (
    <div>
      <div style={{ marginBottom: 28 }}>
        <div style={{ fontSize: 22, fontWeight: 700, color: "var(--text)", letterSpacing: "-.02em" }}>
          Settings
        </div>
      </div>

      <div style={{ display: "flex", gap: 32, alignItems: "flex-start" }}>

        {/* Left nav */}
        <div style={{ width: 180, flexShrink: 0 }}>
          <SectionNav label="Profile"    active={section === "profile"} onClick={() => navSection("profile")} />
          <SectionNav label="GitHub Key" active={section === "github"}  onClick={() => navSection("github")} />
          <SectionNav label="Jira Key"   active={section === "jira"}    onClick={() => navSection("jira")} />
          {userInfo?.role === "admin" && (
            <>
              <div style={{ height: 1, background: "var(--border)", margin: "10px 0" }} />
              <SectionNav label="Users" active={section === "users"} onClick={() => navSection("users")} />
            </>
          )}
        </div>

        {/* Right content */}
        <div style={{ flex: 1, minWidth: 0 }}>
          {section === "profile" && (
            <ProfileSection userInfo={userInfo} onUserUpdate={onUserUpdate} />
          )}
          {section === "github" && (
            <GitHubSection isSet={settings?.github_token_set ?? false} onSaved={reload} />
          )}
          {section === "jira" && (
            <JiraSection
              isSet={settings?.atlassian_token_set ?? false}
              domain={settings?.atlassian_domain ?? ""}
              email={settings?.atlassian_email ?? ""}
              onSaved={reload}
            />
          )}
          {section === "users" && <AdminPage />}
        </div>

      </div>
    </div>
  );
}

// ── Left nav item ──────────────────────────────────────────────────────────────
function SectionNav({ label, active, onClick }: {
  label: string;
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={`settings-nav-item${active ? " active" : ""}`}
    >
      {label}
    </button>
  );
}

// ── Profile section ────────────────────────────────────────────────────────────
function ProfileSection({ userInfo, onUserUpdate }: { userInfo: UserInfo | null; onUserUpdate: (info: UserInfo) => void }) {
  const [username, setUsername] = useState(userInfo?.username ?? "");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (userInfo?.username) setUsername(userInfo.username);
  }, [userInfo?.username]);

  const handleChange = (v: string) => {
    setUsername(v);
    setSaved(false);
    setError(null);
  };

  const handleSave = async () => {
    const trimmed = username.trim();
    if (!trimmed || trimmed === userInfo?.username) return;
    setSaving(true);
    setError(null);
    try {
      const updated = await updateMe(trimmed);
      onUserUpdate(updated);
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to update.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <SectionHeader
        title="Profile"
        description="Update your display name shown in the sidebar and team views."
      />

      <div className="card" style={{ marginTop: 20 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 14, marginBottom: 20, padding: "12px 16px", background: "var(--surface-2)", borderRadius: 8 }}>
          <div style={{
            width: 44, height: 44, borderRadius: "50%", flexShrink: 0,
            background: "var(--accent)", color: "#fff",
            display: "flex", alignItems: "center", justifyContent: "center",
            fontSize: 18, fontWeight: 700,
          }}>
            {(username || userInfo?.username || "U")[0].toUpperCase()}
          </div>
          <div>
            <div style={{ fontSize: "var(--text-base)", fontWeight: 600, color: "var(--text)" }}>
              {username || userInfo?.username || "—"}
            </div>
            <div style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", marginTop: 2 }}>
              {userInfo?.role ?? ""}
            </div>
          </div>
        </div>

        <div className="form-row" style={{ marginBottom: 16 }}>
          <label className="form-label">Display name</label>
          <input
            type="text"
            className="form-input"
            placeholder="Your name"
            value={username}
            onChange={(e) => handleChange(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") handleSave(); }}
            autoComplete="off"
          />
          <span className="form-hint">This is how you appear in the sidebar and task history.</span>
        </div>

        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <button
            className="btn btn-primary"
            onClick={handleSave}
            disabled={saving || !username.trim() || username.trim() === userInfo?.username}
          >
            {saving ? "Saving…" : "Save"}
          </button>
          {saved && <SavedBadge />}
          {error && <span style={{ fontSize: "var(--text-sm)", color: "var(--red)" }}>{error}</span>}
        </div>
      </div>
    </div>
  );
}

// ── GitHub section ─────────────────────────────────────────────────────────────
function GitHubSection({ isSet, onSaved }: { isSet: boolean; onSaved: () => void }) {
  const [token, setToken] = useState("");
  const [showToken, setShowToken] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleChange = (v: string) => {
    setToken(v);
    setSaved(false);
    setError(null);
  };

  const handleSave = async () => {
    if (!token.trim()) return;
    setSaving(true);
    setError(null);
    try {
      await saveSettings({ github_token: token.trim() });
      setToken("");
      setShowToken(false);
      onSaved();
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to save.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <SectionHeader
        title="GitHub Key"
        description="Personal access token used to open pull requests on your behalf. Requires the repo scope."
      />

      <div className="card" style={{ marginTop: 20 }}>
        {isSet && (
          <div className="token-status-banner configured">
            <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
              <polyline points="20 6 9 17 4 12" />
            </svg>
            Token configured — enter a new value below to replace it.
          </div>
        )}

        <div className="form-row" style={{ marginBottom: 16 }}>
          <label className="form-label">Personal Access Token</label>
          <div className="token-input-wrap">
            <input
              type={showToken ? "text" : "password"}
              className="form-input"
              placeholder="ghp_…"
              value={token}
              onChange={(e) => handleChange(e.target.value)}
              onKeyDown={(e) => { if (e.key === "Enter") handleSave(); }}
              autoComplete="off"
            />
            <button
              type="button"
              className="token-visibility-btn"
              onClick={() => setShowToken((v) => !v)}
              title={showToken ? "Hide token" : "Show token"}
            >
              {showToken ? <EyeOffIcon /> : <EyeIcon />}
            </button>
          </div>
          <span className="form-hint">
            Generate one at <strong>github.com → Settings → Developer settings → Personal access tokens</strong>
          </span>
        </div>

        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <button className="btn btn-primary" onClick={handleSave} disabled={saving || !token.trim()}>
            {saving ? "Saving…" : "Save"}
          </button>
          {saved && <SavedBadge />}
          {error && <span style={{ fontSize: "var(--text-sm)", color: "var(--red)" }}>{error}</span>}
        </div>
      </div>
    </div>
  );
}

// ── Jira section ───────────────────────────────────────────────────────────────
function JiraSection({ isSet, domain: savedDomain, email: savedEmail, onSaved }: {
  isSet: boolean;
  domain: string;
  email: string;
  onSaved: () => void;
}) {
  const [token, setToken] = useState("");
  const [showToken, setShowToken] = useState(false);
  const [domain, setDomain] = useState(savedDomain);
  const [email, setEmail] = useState(savedEmail);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => { setDomain(savedDomain); }, [savedDomain]);
  useEffect(() => { setEmail(savedEmail); }, [savedEmail]);

  const markDirty = () => { setSaved(false); setError(null); };

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    try {
      await saveSettings({
        atlassian_token:  token.trim() || undefined,
        atlassian_domain: domain.trim() || undefined,
        atlassian_email:  email.trim() || undefined,
      });
      setToken("");
      setShowToken(false);
      onSaved();
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to save.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <SectionHeader
        title="Jira Key"
        description="Atlassian API token used to fetch Jira ticket context when running tasks with a ticket number."
      />

      <div className="card" style={{ marginTop: 20 }}>
        {isSet && (
          <div className="token-status-banner configured">
            <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
              <polyline points="20 6 9 17 4 12" />
            </svg>
            Token configured — enter a new value below to replace it.
          </div>
        )}

        <div className="form-row" style={{ marginBottom: 16 }}>
          <label className="form-label">Atlassian API Token</label>
          <div className="token-input-wrap">
            <input
              type={showToken ? "text" : "password"}
              className="form-input"
              placeholder="ATATT3x…"
              value={token}
              onChange={(e) => { setToken(e.target.value); markDirty(); }}
              autoComplete="off"
            />
            <button
              type="button"
              className="token-visibility-btn"
              onClick={() => setShowToken((v) => !v)}
              title={showToken ? "Hide token" : "Show token"}
            >
              {showToken ? <EyeOffIcon /> : <EyeIcon />}
            </button>
          </div>
          <span className="form-hint">
            Generate one at <strong>id.atlassian.com → Security → API tokens</strong>
          </span>
        </div>

        <div style={{ display: "flex", gap: 12, marginBottom: 16 }}>
          <div className="form-row" style={{ flex: 1, marginBottom: 0 }}>
            <label className="form-label">Domain</label>
            <input
              type="text"
              className="form-input"
              placeholder="mycompany.atlassian.net"
              value={domain}
              onChange={(e) => { setDomain(e.target.value); markDirty(); }}
            />
          </div>
          <div className="form-row" style={{ flex: 1, marginBottom: 0 }}>
            <label className="form-label">Email</label>
            <input
              type="email"
              className="form-input"
              placeholder="you@company.com"
              value={email}
              onChange={(e) => { setEmail(e.target.value); markDirty(); }}
            />
          </div>
        </div>

        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
            {saving ? "Saving…" : "Save"}
          </button>
          {saved && <SavedBadge />}
          {error && <span style={{ fontSize: "var(--text-sm)", color: "var(--red)" }}>{error}</span>}
        </div>
      </div>
    </div>
  );
}

// ── Shared section header ──────────────────────────────────────────────────────
function SectionHeader({ title, description }: { title: string; description: string }) {
  return (
    <div>
      <h2 style={{ fontSize: 18, fontWeight: 700, color: "var(--text)", letterSpacing: "-.01em", margin: "0 0 6px" }}>
        {title}
      </h2>
      <p style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", margin: 0, lineHeight: 1.6 }}>
        {description}
      </p>
    </div>
  );
}

// ── Persistent save badge (stays until user edits again) ───────────────────────
function SavedBadge() {
  return (
    <span className="saved-badge">
      <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
        <polyline points="20 6 9 17 4 12" />
      </svg>
      Saved
    </span>
  );
}

// ── Eye icons ─────────────────────────────────────────────────────────────────
function EyeIcon() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z" />
      <circle cx="12" cy="12" r="3" />
    </svg>
  );
}

function EyeOffIcon() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24" />
      <line x1="1" y1="1" x2="23" y2="23" />
    </svg>
  );
}
