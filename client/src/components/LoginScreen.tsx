import { useState } from "react";
import { verifyToken } from "../lib/api";

interface Props {
  onLogin: (token: string) => void;
}

export function LoginScreen({ onLogin }: Props) {
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    const trimmed = token.trim();
    if (!trimmed) return;
    setLoading(true);
    try {
      const ok = await verifyToken(trimmed);
      if (!ok) {
        setError("Invalid token. Check with your administrator.");
        return;
      }
      localStorage.setItem("tf_agent_token", trimmed);
      onLogin(trimmed);
    } catch {
      setError("Could not reach server. Is iac-agent running?");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div style={{
      height: "100%", display: "flex", alignItems: "center", justifyContent: "center",
      background: "var(--bg)",
    }}>
      <div style={{ width: 360 }}>
        {/* Brand */}
        <div style={{ textAlign: "center", marginBottom: 32 }}>
          <div style={{
            width: 44, height: 44, borderRadius: 12, background: "var(--accent-subtle)",
            display: "flex", alignItems: "center", justifyContent: "center",
            margin: "0 auto 16px",
          }}>
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="var(--accent)" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
              <polyline points="16 18 22 12 16 6" />
              <polyline points="8 6 2 12 8 18" />
            </svg>
          </div>
          <div style={{ fontSize: "var(--text-xl)", fontWeight: 700, color: "var(--text)", letterSpacing: "-.02em" }}>
            iac-agent
          </div>
          <div style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", marginTop: 4 }}>
            Terraform infrastructure automation
          </div>
        </div>

        {/* Card */}
        <div className="card" style={{ padding: 28 }}>
          <div style={{ marginBottom: 20 }}>
            <div style={{ fontSize: "var(--text-base)", fontWeight: 600, color: "var(--text)" }}>
              Sign in
            </div>
            <div style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", marginTop: 2 }}>
              Enter your API token to continue
            </div>
          </div>

          <form onSubmit={handleSubmit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            <div className="form-row">
              <label className="form-label">API Token</label>
              <input
                type="password"
                className="form-input"
                placeholder="••••••••••••••••"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                autoFocus
              />
              {error && (
                <span style={{ fontSize: "var(--text-xs)", color: "var(--red)" }}>{error}</span>
              )}
            </div>

            <button
              type="submit"
              className="btn btn-primary"
              disabled={loading || !token.trim()}
              style={{ width: "100%", justifyContent: "center", padding: "10px 18px" }}
            >
              {loading ? "Verifying…" : "Continue →"}
            </button>
          </form>
        </div>

        <div style={{ marginTop: 14, textAlign: "center", fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          Token stored in browser only · never sent to third parties
        </div>
      </div>
    </div>
  );
}
