import type { ModelsResponse, TaskFormValues } from "../types";

const getToken = () => localStorage.getItem("tf_agent_token") ?? "";

const authHeaders = () => ({
  "Content-Type": "application/json",
  Authorization: `Bearer ${getToken()}`,
});

export async function submitTask(form: TaskFormValues): Promise<{ task_id: string }> {
  // Prefix task with jira ticket and options context.
  let text = form.task.trim();
  if (form.jiraTicket.trim()) {
    text = `Jira: ${form.jiraTicket.trim()}\n\n${text}`;
  }
  if (form.dryRun) {
    text = `[DRY RUN — generate and show code only, do not open a PR]\n\n${text}`;
  }
  const repos = form.comprehensionRepos.filter((r) => r.trim());
  if (repos.length > 0) {
    text = `[Scan these repos for existing Terraform patterns before generating: ${repos.join(", ")}]\n\n${text}`;
  }

  const res = await fetch("/v1/tasks", {
    method: "POST",
    headers: authHeaders(),
    body: JSON.stringify({
      input: { type: "prompt", text },
      output: {
        type: form.dryRun ? "print" : (form.workRepo.trim() ? "pr" : "print"),
        repo_url: form.workRepo.trim() || undefined,
      },
      model: form.model || undefined,
    }),
  });

  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to submit task");
  }

  return res.json();
}

export async function listModels(): Promise<ModelsResponse> {
  const res = await fetch("/v1/models", { headers: authHeaders() });
  if (!res.ok) throw new Error("Failed to load models");
  return res.json();
}

export async function getTask(id: string): Promise<import("../types").TaskDetail> {
  const res = await fetch(`/v1/tasks/${id}`, { headers: authHeaders() });
  if (!res.ok) throw new Error("Task not found");
  return res.json();
}

export async function listTasks(): Promise<import("../types").TaskDetail[]> {
  const res = await fetch("/v1/tasks", { headers: authHeaders() });
  if (!res.ok) throw new Error("Failed to load tasks");
  return res.json();
}

export function streamTask(taskId: string): EventSource {
  const token = getToken();
  return new EventSource(`/v1/tasks/${taskId}/stream?token=${encodeURIComponent(token)}`);
}

export async function cancelTask(taskId: string): Promise<void> {
  await fetch(`/v1/tasks/${taskId}/cancel`, {
    method: "POST",
    headers: authHeaders(),
  });
}

export async function answerTask(taskId: string, answer: string): Promise<void> {
  await fetch(`/v1/tasks/${taskId}/answer`, {
    method: "POST",
    headers: authHeaders(),
    body: JSON.stringify({ answer }),
  });
}

export async function respondToPermission(taskId: string, allow: boolean): Promise<void> {
  await fetch(`/v1/tasks/${taskId}/permission`, {
    method: "POST",
    headers: authHeaders(),
    body: JSON.stringify({ allow }),
  });
}

export async function verifyToken(token: string): Promise<boolean> {
  const res = await fetch("/v1/tasks", {
    headers: { Authorization: `Bearer ${token}` },
  });
  return res.ok;
}

export async function updateMe(username: string): Promise<import("../types").UserInfo> {
  const res = await fetch("/v1/me", {
    method: "PATCH",
    headers: authHeaders(),
    body: JSON.stringify({ username }),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to update username");
  }
  return res.json();
}

export async function getMe(token?: string): Promise<import("../types").UserInfo> {
  const headers = token
    ? { Authorization: `Bearer ${token}` }
    : { Authorization: `Bearer ${getToken()}` };
  const res = await fetch("/v1/me", { headers });
  if (!res.ok) throw new Error("Unauthorized");
  return res.json();
}

export async function getSettings(): Promise<import("../types").UserSettings> {
  const res = await fetch("/v1/settings", { headers: authHeaders() });
  if (!res.ok) throw new Error("Failed to load settings");
  return res.json();
}

export async function listAdminUsers(): Promise<import("../types").AdminUser[]> {
  const res = await fetch("/v1/admin/users", { headers: authHeaders() });
  if (!res.ok) throw new Error("Failed to list users");
  return res.json();
}

export async function createAdminUser(data: {
  username: string;
  token: string;
  role: "admin" | "member";
}): Promise<{ id: string; username: string; role: string }> {
  const res = await fetch("/v1/admin/users", {
    method: "POST",
    headers: authHeaders(),
    body: JSON.stringify(data),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to create user");
  }
  return res.json();
}

export async function updateAdminUser(id: string, data: { username: string; role: "admin" | "member" }): Promise<{ id: string; username: string; role: string }> {
  const res = await fetch(`/v1/admin/users/${id}`, {
    method: "PATCH",
    headers: authHeaders(),
    body: JSON.stringify(data),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to update user");
  }
  return res.json();
}

export async function deleteAdminUser(id: string): Promise<void> {
  const res = await fetch(`/v1/admin/users/${id}`, { method: "DELETE", headers: authHeaders() });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to delete user");
  }
}

export async function setAdminUserActive(id: string, active: boolean): Promise<void> {
  const res = await fetch(`/v1/admin/users/${id}/${active ? "activate" : "deactivate"}`, {
    method: "POST",
    headers: authHeaders(),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to update user status");
  }
}

export async function revokeAdminUserToken(id: string): Promise<void> {
  const res = await fetch(`/v1/admin/users/${id}/token`, { method: "DELETE", headers: authHeaders() });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to revoke token");
  }
}

export async function regenerateAdminUserToken(id: string): Promise<{ token: string }> {
  const res = await fetch(`/v1/admin/users/${id}/token`, {
    method: "POST",
    headers: authHeaders(),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to regenerate token");
  }
  return res.json();
}

export async function saveSettings(data: {
  github_token?: string;
  atlassian_token?: string;
  atlassian_domain?: string;
  atlassian_email?: string;
}): Promise<void> {
  // Strip undefined values so they don't serialize as "null"
  const body = Object.fromEntries(Object.entries(data).filter(([, v]) => v !== undefined));
  const res = await fetch("/v1/settings", {
    method: "PUT",
    headers: authHeaders(),
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "Failed to save settings");
  }
}
