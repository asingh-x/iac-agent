import { useEffect, useState } from "react";
import { getSettings } from "../lib/api";
import type { UserSettings } from "../types";

const SPINNER_FRAMES = ["⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"];

const PIPELINE = [
  { label: "Task created" },
  { label: "Comprehension" },
  { label: "Code generation" },
  { label: "Validation" },
  { label: "Security scan" },
  { label: "PR opened ↗" },
];

// How long each step stays "active" before ticking to done (ms)
const STEP_DURATIONS = [500, 1100, 1600, 900, 800, 1000];
const PR_DURATION    = 5 * 60 * 1000; // 5 min pause before reset

const INTEGRATIONS: { key: keyof Pick<UserSettings, "github_token_set" | "atlassian_token_set">; label: string; section: string }[] = [
  { key: "github_token_set",    label: "GitHub", section: "github" },
  { key: "atlassian_token_set", label: "Jira",   section: "jira"   },
];

interface Props {
  onNavigate?: (page: string) => void;
}

export function HeroPage({ onNavigate }: Props) {
  const [settings, setSettings]       = useState<UserSettings | null>(null);
  const [phase, setPhase]             = useState(0);
  const [spinnerFrame, setFrame]      = useState(0);
  const [hoveredChip, setHoveredChip] = useState<string | null>(null);

  useEffect(() => {
    getSettings().then(setSettings).catch(() => {});
  }, []);

  // Braille spinner — only tick while a step is actively running
  useEffect(() => {
    const animating = phase < PIPELINE.length;
    if (!animating) return;
    const id = setInterval(() => setFrame((f) => (f + 1) % SPINNER_FRAMES.length), 100);
    return () => clearInterval(id);
  }, [phase]);

  // Advance through phases
  useEffect(() => {
    const duration = phase < PIPELINE.length ? STEP_DURATIONS[phase] : PR_DURATION;
    const id = setTimeout(() => {
      setPhase((p) => (p + 1) % (PIPELINE.length + 1));
    }, duration);
    return () => clearTimeout(id);
  }, [phase]);

  const allDone = phase >= PIPELINE.length;

  return (
    <div style={{ animation: "slideUp .25s ease" }}>
      {/* Hero */}
      <div style={{ paddingTop: 24, paddingBottom: 40 }}>
        <h1 style={{
          fontSize: 32, fontWeight: 700, color: "var(--text)",
          letterSpacing: "-.03em", lineHeight: 1.2, marginBottom: 14,
        }}>
          Generate Terraform<br />infrastructure, instantly.
        </h1>
        <p style={{ fontSize: "var(--text-base)", color: "var(--text-3)", lineHeight: 1.75, maxWidth: 460 }}>
          Describe what you need. tf-agent scans your repos for existing patterns,
          writes HCL, runs{" "}
          <code style={{ fontFamily: "var(--font-mono)", fontSize: 13, color: "var(--text-2)", background: "var(--surface-2)", padding: "1px 5px", borderRadius: 4 }}>
            terraform validate
          </code>
          , and opens a pull request — autonomously.
        </p>
      </div>

      {/* Animated pipeline */}
      <div style={{ borderTop: "1px solid var(--border)", paddingTop: 24, paddingBottom: 24 }}>
        <div style={{ fontSize: "var(--text-xs)", fontWeight: 600, color: "var(--text-3)", textTransform: "uppercase", letterSpacing: ".08em", marginBottom: 14 }}>
          How it works
        </div>

        {/* Node + connector row */}
        <div style={{ display: "flex", alignItems: "center" }}>
          {PIPELINE.map((step, i) => {
            const isDone   = i < phase || allDone;
            const isActive = i === phase && !allDone;
            const isLast   = i === PIPELINE.length - 1;
            const lineColor = isDone ? "var(--accent)" : "var(--border)";

            return (
              <div key={step.label} style={{ display: "flex", alignItems: "center", flex: 1 }}>
                {/* Node */}
                <div style={{ width: 16, height: 16, flexShrink: 0, display: "flex", alignItems: "center", justifyContent: "center" }}>
                  {isActive ? (
                    <span style={{ fontSize: 10, lineHeight: 1, color: "var(--accent)" }}>
                      {SPINNER_FRAMES[spinnerFrame]}
                    </span>
                  ) : (
                    <div style={{
                      width: 7, height: 7, borderRadius: "50%",
                      background: isDone ? "var(--accent)" : "var(--border-strong)",
                      transition: "background .3s",
                    }} />
                  )}
                </div>

                {/* Connector line */}
                {!isLast && (
                  <div style={{ flex: 1, height: 1, background: lineColor, transition: "background .4s" }} />
                )}
              </div>
            );
          })}
        </div>

        {/* Label row */}
        <div style={{ display: "flex", marginTop: 8 }}>
          {PIPELINE.map((step, i) => {
            const isDone   = i < phase || allDone;
            const isActive = i === phase && !allDone;
            const isPending = !isDone && !isActive;

            return (
              <div key={step.label} style={{ flex: 1, minWidth: 0 }}>
                <span style={{
                  fontSize: "var(--text-xs)", fontWeight: isActive ? 600 : 500,
                  color: isDone ? "var(--accent)" : isActive ? "var(--text)" : "var(--text-3)",
                  opacity: isPending ? 0.45 : 1,
                  transition: "color .3s, opacity .4s",
                  display: "block", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap",
                }}>
                  {step.label}
                </span>
              </div>
            );
          })}
        </div>
      </div>

      {/* Integrations */}
      <div style={{ borderTop: "1px solid var(--border)", paddingTop: 20 }}>
        <div style={{ fontSize: "var(--text-xs)", fontWeight: 600, color: "var(--text-3)", textTransform: "uppercase", letterSpacing: ".08em", marginBottom: 10 }}>
          Integrations
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          {INTEGRATIONS.map(({ key, label, section }) => {
            const configured = settings ? settings[key] : false;
            const isHovered  = hoveredChip === label;
            const clickable  = !!onNavigate;

            return (
              <div
                key={label}
                title={configured ? `${label} connected` : `${label} not configured — click to set up`}
                onClick={clickable ? () => { sessionStorage.setItem("tf_settings_section", section); onNavigate!("settings"); } : undefined}
                onMouseEnter={clickable ? () => setHoveredChip(label) : undefined}
                onMouseLeave={clickable ? () => setHoveredChip(null) : undefined}
                style={{
                  display: "inline-flex", flexDirection: "column", gap: 2,
                  background: configured ? "var(--green-bg)" : isHovered ? "var(--surface-2)" : "var(--surface)",
                  border: `1px solid ${configured ? "#a7d9bc" : isHovered ? "var(--border-strong)" : "var(--border-strong)"}`,
                  borderRadius: 10, padding: "7px 12px",
                  boxShadow: "0 1px 2px rgba(0,0,0,.04)",
                  cursor: clickable ? "pointer" : "default",
                  transition: "background .15s",
                }}
              >
                <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
                  <span style={{
                    width: 6, height: 6, borderRadius: "50%", flexShrink: 0,
                    background: configured ? "var(--green)" : "var(--border-strong)",
                  }} />
                  <span style={{ fontSize: "var(--text-xs)", fontWeight: 600, color: configured ? "var(--green)" : "var(--text-3)" }}>
                    {label}
                  </span>
                </div>
                {configured && (
                  <span style={{ fontSize: "var(--text-xs)", color: "#5a9970", paddingLeft: 12 }}>
                    Authenticated
                  </span>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
