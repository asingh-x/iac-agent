import { useEffect, useRef, useState } from "react";
import type { UserInfo } from "../types";

interface Props {
  onLogout: () => void;
  userInfo: UserInfo | null;
  onNewTask: () => void;
  onGoHome: () => void;
  onHistory: () => void;
  onSettings: () => void;
  activeView: string;
}

export function Sidebar({
  onLogout, userInfo,
  onNewTask, onGoHome, onHistory, onSettings, activeView,
}: Props) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  // Close menu on outside click
  useEffect(() => {
    if (!menuOpen) return;
    const handler = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setMenuOpen(false);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [menuOpen]);

  const avatarLetter = userInfo?.username?.[0]?.toUpperCase() ?? "U";

  return (
    <aside className="sidebar" style={{
      width: 240, flexShrink: 0,
      background: "var(--sidebar-bg)", borderRight: "1px solid var(--border)",
      display: "flex", flexDirection: "column", overflow: "hidden",
    }}>
      {/* Brand */}
      <div className="sidebar-brand" onClick={onGoHome} style={{ cursor: "pointer" }} title="Home">
        <div style={{
          width: 30, height: 30, borderRadius: "var(--radius-sm)", flexShrink: 0,
          background: "var(--accent-subtle)",
          display: "flex", alignItems: "center", justifyContent: "center",
        }}>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="var(--accent)" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M9.5 3.5c-1.4 0-2.3.9-2.3 2.3v2.8c0 1.2-.6 1.9-1.9 2.4 1.3.5 1.9 1.2 1.9 2.4v2.8c0 1.4.9 2.3 2.3 2.3"/>
            <path d="M14.5 3.5c1.4 0 2.3.9 2.3 2.3v2.8c0 1.2.6 1.9 1.9 2.4-1.3.5-1.9 1.2-1.9 2.4v2.8c0 1.4-.9 2.3-2.3 2.3"/>
            <circle cx="10.3" cy="12" r="1.2" fill="var(--accent)" stroke="none"/>
            <circle cx="13.7" cy="12" r="1.2" fill="var(--accent)" stroke="none"/>
          </svg>
        </div>
        <div>
          <div className="sidebar-brand-name">IAC Agent</div>
          <div className="sidebar-brand-tag">Terraform automation</div>
        </div>
      </div>

      {/* Nav items */}
      <div style={{ padding: "10px 10px 4px", display: "flex", flexDirection: "column", gap: 2 }}>
        {/* New task */}
        <button
          onClick={onNewTask}
          style={{
            display: "flex", alignItems: "center", gap: 8,
            width: "100%", padding: "7px 10px", borderRadius: "var(--radius-sm)",
            fontSize: "var(--text-sm)", fontWeight: 500,
            color: activeView === "form" ? "var(--accent)" : "var(--text-3)",
            background: activeView === "form" ? "var(--accent-subtle)" : "transparent",
            border: activeView === "form" ? "1px solid var(--nav-active-border)" : "1px solid transparent",
            cursor: "pointer", transition: "background .12s, color .12s",
          }}
          onMouseEnter={e => { if (activeView !== "form") e.currentTarget.style.background = "var(--surface-2)"; }}
          onMouseLeave={e => { if (activeView !== "form") e.currentTarget.style.background = "transparent"; }}
        >
          <svg width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2.2" viewBox="0 0 24 24">
            <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
          </svg>
          New task
        </button>

        {/* Recent Tasks nav item */}
        <NavItem
          label="Recent Tasks"
          active={activeView === "history"}
          onClick={onHistory}
          icon={
            <svg width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
              <path d="M3 3v5h5"/><path d="M3.05 13A9 9 0 1 0 6 5.3L3 8"/>
            </svg>
          }
        />

      </div>

      {/* Spacer */}
      <div style={{ flex: 1 }} />

      {/* Footer — user badge + account menu */}
      <div ref={menuRef} style={{ position: "relative", borderTop: "1px solid var(--border)" }}>
        {/* Account popup menu */}
        {menuOpen && (
          <div style={{
            position: "absolute", bottom: "100%", left: 12, right: 12,
            background: "var(--menu-bg)",
            backdropFilter: "blur(20px) saturate(180%)",
            WebkitBackdropFilter: "blur(20px) saturate(180%)",
            border: "1px solid var(--border)",
            borderRadius: "var(--radius-sm)", boxShadow: "0 -4px 20px rgba(0,0,0,.1)",
            overflow: "hidden", animation: "slideUp .15s ease",
          }}>
            {/* Username header */}
            <div style={{
              display: "flex", alignItems: "center", gap: 9,
              padding: "12px 14px",
              borderBottom: "1px solid var(--border)",
            }}>
              <div style={{
                width: 30, height: 30, borderRadius: "50%", flexShrink: 0,
                background: "var(--accent)", color: "#fff",
                display: "flex", alignItems: "center", justifyContent: "center",
                fontSize: 13, fontWeight: 700,
              }}>
                {avatarLetter}
              </div>
              <div>
                <div style={{ fontSize: "var(--text-sm)", fontWeight: 600, color: "var(--text)" }}>
                  {userInfo?.username ?? "—"}
                </div>
                <div style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                  {userInfo?.role ?? ""}
                </div>
              </div>
            </div>

            {/* Menu items */}
            <div style={{ padding: "6px 0" }}>
              <MenuRow
                label="Settings"
                onClick={() => { setMenuOpen(false); onSettings(); }}
                icon={
                  <svg width="13" height="13" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                    <circle cx="12" cy="12" r="3"/>
                    <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>
                  </svg>
                }
              />
            </div>

            {/* Divider + sign out */}
            <div style={{ borderTop: "1px solid var(--border)", padding: "6px 0" }}>
              <MenuRow
                label="Log out"
                onClick={() => { setMenuOpen(false); onLogout(); }}
                danger
                icon={
                  <svg width="13" height="13" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                    <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/>
                    <polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/>
                  </svg>
                }
              />
            </div>
          </div>
        )}

        {/* User badge button */}
        <button
          className="sidebar-user-btn"
          onClick={() => setMenuOpen((o) => !o)}
          style={{
            display: "flex", alignItems: "center", gap: 9,
            width: "100%", padding: "12px 14px",
            background: menuOpen ? "var(--surface-2, #f5f4f0)" : "transparent",
            border: "none", cursor: "pointer", transition: "background .12s", textAlign: "left",
          }}
          onMouseEnter={e => { if (!menuOpen) e.currentTarget.style.background = "var(--surface-2, #f5f4f0)"; }}
          onMouseLeave={e => { if (!menuOpen) e.currentTarget.style.background = "transparent"; }}
        >
          <div style={{
            width: 28, height: 28, borderRadius: "50%", flexShrink: 0,
            background: "var(--accent)", color: "#fff",
            display: "flex", alignItems: "center", justifyContent: "center",
            fontSize: 12, fontWeight: 700,
          }}>
            {avatarLetter}
          </div>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{ fontSize: "var(--text-sm)", fontWeight: 600, color: "var(--text)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
              {userInfo?.username ?? "—"}
            </div>
            {userInfo?.role && (
              <div style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>{userInfo.role}</div>
            )}
          </div>
          <svg
            width="12" height="12" fill="none" stroke="var(--text-3)" strokeWidth="2" viewBox="0 0 24 24"
            style={{ flexShrink: 0, transform: menuOpen ? "rotate(180deg)" : "none", transition: "transform .15s" }}
          >
            <polyline points="18 15 12 9 6 15"/>
          </svg>
        </button>
      </div>
    </aside>
  );
}

function NavItem({ label, active, onClick, icon }: { label: string; active: boolean; onClick: () => void; icon: React.ReactNode }) {
  return (
    <button
      onClick={onClick}
      style={{
        display: "flex", alignItems: "center", gap: 8,
        width: "100%", padding: "7px 10px", borderRadius: "var(--radius-sm)",
        fontSize: "var(--text-sm)", fontWeight: 500,
        color: active ? "var(--accent)" : "var(--text-3)",
        background: active ? "var(--accent-subtle)" : "transparent",
        border: active ? "1px solid var(--nav-active-border)" : "1px solid transparent",
        cursor: "pointer", transition: "background .12s, color .12s",
      }}
      onMouseEnter={e => { if (!active) e.currentTarget.style.background = "var(--surface-2, #f5f4f0)"; }}
      onMouseLeave={e => { if (!active) e.currentTarget.style.background = "transparent"; }}
    >
      {icon}
      {label}
    </button>
  );
}

function MenuRow({ label, onClick, icon, danger }: { label: string; onClick: () => void; icon: React.ReactNode; danger?: boolean }) {
  return (
    <button
      onClick={onClick}
      style={{
        display: "flex", alignItems: "center", gap: 10,
        width: "100%", padding: "9px 14px",
        background: "none", border: "none", cursor: "pointer",
        fontSize: "var(--text-sm)", color: danger ? "var(--red, #c0392b)" : "var(--text-2)",
        transition: "background .1s",
        textAlign: "left",
      }}
      onMouseEnter={e => { e.currentTarget.style.background = danger ? "var(--red-bg, #fdf0ee)" : "var(--surface-2, #f5f4f0)"; }}
      onMouseLeave={e => { e.currentTarget.style.background = "none"; }}
    >
      <span style={{ color: danger ? "var(--red, #c0392b)" : "var(--text-3)", flexShrink: 0 }}>{icon}</span>
      {label}
    </button>
  );
}

