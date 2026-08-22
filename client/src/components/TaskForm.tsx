import { useEffect, useState } from "react";
import { listModels } from "../lib/api";
import type { ModelInfo, TaskFormValues } from "../types";

interface Props {
  onSubmit: (values: TaskFormValues) => void;
  loading: boolean;
}

const DEFAULT: TaskFormValues = {
  task: "",
  jiraTicket: "",
  workRepo: "",
  comprehensionRepos: [""],
  dryRun: false,
  model: "",
};

export function TaskForm({ onSubmit, loading }: Props) {
  const [form, setForm] = useState<TaskFormValues>(DEFAULT);
  const [models, setModels] = useState<ModelInfo[]>([]);

  useEffect(() => {
    listModels()
      .then((res) => {
        setModels(res.models);
        const def = res.models.find((m) => m.default);
        if (def) setForm((f) => ({ ...f, model: def.id }));
      })
      .catch(() => {});
  }, []);

  const set = <K extends keyof TaskFormValues>(key: K, value: TaskFormValues[K]) =>
    setForm((f) => ({ ...f, [key]: value }));

  const setRepo = (index: number, value: string) => {
    const repos = [...form.comprehensionRepos];
    repos[index] = value;
    set("comprehensionRepos", repos);
  };

  const addRepo = () => {
    if (form.comprehensionRepos.length >= 2) return;
    set("comprehensionRepos", [...form.comprehensionRepos, ""]);
  };

  const removeRepo = (index: number) => {
    const repos = form.comprehensionRepos.filter((_, i) => i !== index);
    set("comprehensionRepos", repos.length > 0 ? repos : [""]);
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!form.task.trim() || loading) return;
    onSubmit(form);
  };

  return (
    <form onSubmit={handleSubmit} style={{ display: "flex", flexDirection: "column", gap: 20 }}>

      {/* Task */}
      <div className="form-row">
        <label className="form-label">Task description</label>
        <textarea
          className="form-input"
          rows={4}
          placeholder="Describe the infrastructure you need — e.g. an S3 bucket with versioning and lifecycle rules for the payments team."
          value={form.task}
          onChange={(e) => set("task", e.target.value)}
          disabled={loading}
        />
      </div>

      {/* Jira Ticket */}
      <div className="form-row">
        <label className="form-label">
          Jira ticket
          <span style={{ fontWeight: 400, color: "var(--text-3)", marginLeft: 5 }}>optional</span>
        </label>
        <input
          type="text"
          className="form-input"
          placeholder="INFRA-123"
          value={form.jiraTicket}
          onChange={(e) => set("jiraTicket", e.target.value)}
          disabled={loading}
        />
        <span className="form-hint">Ticket context will be fetched and added to the agent prompt.</span>
      </div>

      {/* Repo Comprehension */}
      <div className="form-row">
        <label className="form-label">Repo comprehension</label>
        <span className="form-hint" style={{ marginBottom: 6 }}>
          Repos to scan for existing Terraform patterns, modules, and naming conventions.
        </span>
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          {form.comprehensionRepos.map((repo, i) => (
            <div key={i} style={{ display: "flex", gap: 6 }}>
              <input
                type="text"
                className="form-input"
                placeholder={i === 0 ? "github.com/org/infra-modules" : "github.com/org/platform-tf"}
                value={repo}
                onChange={(e) => setRepo(i, e.target.value)}
                disabled={loading}
                style={{ flex: 1 }}
              />
              {form.comprehensionRepos.length > 1 && (
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => removeRepo(i)}
                  disabled={loading}
                  style={{ flexShrink: 0 }}
                >
                  ✕
                </button>
              )}
            </div>
          ))}
          {form.comprehensionRepos.length < 2 && (
            <button
              type="button"
              className="btn btn-sm"
              onClick={addRepo}
              disabled={loading}
              style={{ alignSelf: "flex-start" }}
            >
              + Add repo
            </button>
          )}
        </div>
      </div>

      {/* Work Repo */}
      <div className="form-row">
        <label className="form-label">Work repo</label>
        <input
          type="text"
          className="form-input"
          placeholder="github.com/org/infra"
          value={form.workRepo}
          onChange={(e) => set("workRepo", e.target.value)}
          disabled={loading}
        />
        <span className="form-hint">GitHub repo where generated files will be committed and a PR opened.</span>
      </div>

      {/* Dry Run */}
      <div className="form-row">
        <label className="form-label">Options</label>
        <Toggle
          checked={form.dryRun}
          onChange={(v) => set("dryRun", v)}
          disabled={loading}
          label="Dry run"
          hint="Preview generated code only — no PR will be opened."
        />
      </div>

      {/* Model */}
      {models.length > 0 && (
        <div className="form-row">
          <label className="form-label">Model</label>
          <select
            className="form-input"
            value={form.model}
            onChange={(e) => set("model", e.target.value)}
            disabled={loading}
            style={{ cursor: "pointer" }}
          >
            {models.map((m) => (
              <option key={m.id} value={m.id}>{m.name}</option>
            ))}
          </select>
        </div>
      )}

      {/* Submit */}
      <div style={{ display: "flex", alignItems: "center", gap: 12, paddingTop: 4 }}>
        <button
          type="submit"
          className="btn btn-primary"
          disabled={loading || !form.task.trim()}
          style={{ minWidth: 148 }}
        >
          {loading ? "Agent running…" : "Run iac-agent →"}
        </button>
        {form.dryRun && (
          <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            Code preview only · no PR will be opened
          </span>
        )}
      </div>
    </form>
  );
}

interface ToggleProps {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  label: string;
  hint: string;
}

function Toggle({ checked, onChange, disabled, label, hint }: ToggleProps) {
  return (
    <label style={{
      display: "flex", alignItems: "flex-start", gap: 10,
      cursor: disabled ? "not-allowed" : "pointer",
      opacity: disabled ? 0.5 : 1,
    }}>
      <div
        onClick={() => !disabled && onChange(!checked)}
        style={{
          width: 32, height: 18, borderRadius: 9, flexShrink: 0, marginTop: 2,
          background: checked ? "var(--accent)" : "var(--border-strong)",
          border: `1px solid ${checked ? "var(--accent)" : "var(--border-strong)"}`,
          position: "relative",
          transition: "background .15s, border-color .15s",
          cursor: disabled ? "not-allowed" : "pointer",
        }}
      >
        <div style={{
          position: "absolute", top: 2, left: checked ? 15 : 2,
          width: 12, height: 12, borderRadius: "50%",
          background: checked ? "#fff" : "var(--surface)",
          boxShadow: "0 1px 2px rgba(0,0,0,.2)",
          transition: "left .15s",
        }} />
      </div>
      <div>
        <div style={{ fontSize: "var(--text-sm)", color: "var(--text-2)", fontWeight: 500 }}>{label}</div>
        <div style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", marginTop: 1 }}>{hint}</div>
      </div>
    </label>
  );
}
