import { useEffect, useState } from "react";
import { TaskForm } from "./TaskForm";
import { getSettings } from "../lib/api";
import type { TaskFormValues, UserSettings } from "../types";

interface Props {
  onSubmit: (values: TaskFormValues) => void;
  loading: boolean;
  onNavigate?: (page: string) => void;
}

const FEATURES = [
  { icon: "⎇", label: "Repo comprehension" },
  { icon: "◈", label: "HCL generation" },
  { icon: "✓", label: "Terraform validate" },
  { icon: "⎇", label: "Auto PR" },
];

const INTEGRATIONS: { key: keyof Pick<UserSettings, "github_token_set" | "atlassian_token_set">; label: string; section: string }[] = [
  { key: "github_token_set",    label: "GitHub", section: "github" },
  { key: "atlassian_token_set", label: "Jira",   section: "jira"   },
];

export function HomePage({ onSubmit, loading, onNavigate }: Props) {
  const [settings, setSettings] = useState<UserSettings | null>(null);

  useEffect(() => {
    getSettings().then(setSettings).catch(() => {});
  }, []);

  return (
    <div>
      {/* Hero */}
      <div style={{ marginBottom: 36, paddingBottom: 32, borderBottom: "1px solid var(--border)" }}>
        <h1 style={{
          fontSize: 28, fontWeight: 700, color: "var(--text)",
          letterSpacing: "-.03em", lineHeight: 1.2, marginBottom: 10,
        }}>
          Generate Terraform<br />infrastructure, instantly.
        </h1>

        <p style={{
          fontSize: "var(--text-base)", color: "var(--text-3)",
          lineHeight: 1.7, maxWidth: 480, marginBottom: 20,
        }}>
          Describe what you need. tf-agent scans your repos for patterns,
          writes HCL, runs <code style={{ fontFamily: "var(--font-mono)", fontSize: 13, color: "var(--text-2)" }}>terraform validate</code>,
          and opens a pull request — all autonomously.
        </p>

        <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
          {FEATURES.map((f) => (
            <span key={f.label} style={{
              display: "inline-flex", alignItems: "center", gap: 5,
              fontSize: "var(--text-xs)", fontWeight: 500,
              color: "var(--text-2)", background: "var(--surface)",
              border: "1px solid var(--border)", borderRadius: 20,
              padding: "4px 10px", boxShadow: "0 1px 2px rgba(0,0,0,.04)",
            }}>
              <span style={{ fontSize: 10, color: "var(--accent)" }}>{f.icon}</span>
              {f.label}
            </span>
          ))}
          {settings && INTEGRATIONS.map(({ key, label, section }) => {
            const configured = settings[key];
            return (
              <span
                key={label}
                title={configured ? `${label} token configured` : `${label} token not set — click to configure`}
                onClick={onNavigate ? () => { sessionStorage.setItem("tf_settings_section", section); onNavigate("settings"); } : undefined}
                style={{
                  display: "inline-flex", alignItems: "center", gap: 5,
                  fontSize: "var(--text-xs)", fontWeight: 500,
                  color: configured ? "var(--green)" : "var(--text-3)",
                  background: configured ? "var(--green-bg)" : "var(--surface)",
                  border: `1px solid ${configured ? "#a7d9bc" : "var(--border)"}`,
                  borderRadius: 20, padding: "4px 10px",
                  boxShadow: "0 1px 2px rgba(0,0,0,.04)",
                  cursor: onNavigate ? "pointer" : "default",
                }}>
                <span style={{
                  width: 6, height: 6, borderRadius: "50%",
                  background: configured ? "var(--green)" : "var(--border-strong)",
                  flexShrink: 0,
                }} />
                {label}
              </span>
            );
          })}
        </div>
      </div>

      {/* Form card */}
      <div className="card">
        <div style={{ marginBottom: 20 }}>
          <div style={{ fontSize: "var(--text-md)", fontWeight: 600, color: "var(--text)", letterSpacing: "-.01em" }}>
            New task
          </div>
          <div style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", marginTop: 3 }}>
            Describe the infrastructure you want to generate.
          </div>
        </div>
        <TaskForm onSubmit={onSubmit} loading={loading} />
      </div>
    </div>
  );
}
