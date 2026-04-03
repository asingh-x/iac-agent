export interface UserInfo {
  id: string;
  username: string;
  role: "admin" | "member";
}

export interface AdminUser {
  id: string;
  username: string;
  role: "admin" | "member";
  active: boolean;
  created_at: string;
}

export interface UserSettings {
  github_token_set: boolean;
  atlassian_token_set: boolean;
  atlassian_domain: string;
  atlassian_email: string;
}

export type SSEEventType = "text" | "tool_start" | "tool_end" | "done" | "error" | "status" | "waiting_for_input";

export interface SSEEvent {
  type: SSEEventType;
  text?: string;
  tool?: string;
  output?: string;
  pr_url?: string;
  status?: string;
  error?: string;
}

export interface ModelInfo {
  id: string;
  name: string;
  default?: boolean;
}

export interface ModelsResponse {
  provider: string;
  models: ModelInfo[];
}

export interface TaskFormValues {
  task: string;
  jiraTicket: string;
  workRepo: string;
  comprehensionRepos: string[];  // repos to scan for patterns
  dryRun: boolean;
  model: string;
}

export interface OutputLine {
  id: string;
  kind: "text" | "tool_start" | "tool_end" | "error" | "status";
  content: string;
  toolName?: string;
}

export interface HistoryTask {
  id: string;
  status: "queued" | "running" | "done" | "failed" | "waiting_for_input" | "cancelled";
  input_text: string;
  created_at: string;
  pr_url?: string;
  pending_question?: string;
}

export interface TaskDetail extends HistoryTask {
  output_type: string;
  input_tokens: number;
  output_tokens: number;
  error_msg?: string;
  output?: string;
  started_at?: string;
  completed_at?: string;
}
