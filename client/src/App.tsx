import { useEffect, useState } from "react";
import { LoginScreen } from "./components/LoginScreen";
import { Sidebar } from "./components/Sidebar";
import { HeroPage } from "./components/HeroPage";
import { TaskForm } from "./components/TaskForm";
import { OutputPanel } from "./components/OutputPanel";
import { TaskDetail } from "./components/TaskDetail";
import { HistoryPage } from "./components/HistoryPage";
import { SettingsPage } from "./components/SettingsPage";
import { useTaskRunner } from "./hooks/useTaskRunner";
import { getMe } from "./lib/api";
import type { TaskFormValues, HistoryTask, UserInfo } from "./types";

type View = "landing" | "form" | "history" | "settings";

const VIEW_TITLE: Record<View, string> = {
  landing:  "Home",
  form:     "New task",
  history:  "Recent tasks",
  settings: "Settings",
};

const SS_TASK = "tf_selected_task_id";

const HASH_TO_VIEW: Record<string, View> = {
  "#history":  "history",
  "#form":     "form",
  "#settings": "settings",
};
const VIEW_TO_HASH: Record<View, string> = {
  landing:  "",
  form:     "#form",
  history:  "#history",
  settings: "#settings",
};

function viewFromHash(): View {
  return HASH_TO_VIEW[window.location.hash] ?? "landing";
}

function pushHash(v: View) {
  const h = VIEW_TO_HASH[v];
  if (window.location.hash !== h) {
    window.location.hash = h;
  }
}

export default function App() {
  const [token, setToken] = useState(() => localStorage.getItem("tf_agent_token") ?? "");
  const [userInfo, setUserInfo] = useState<UserInfo | null>(null);
  const [refreshTrigger, setRefreshTrigger] = useState(0);
  const [selectedTask, setSelectedTask] = useState<HistoryTask | null>(() => {
    const id = sessionStorage.getItem(SS_TASK);
    return id ? { id, status: "done", input_text: "", created_at: "" } : null;
  });
  const [view, setView] = useState<View>(viewFromHash);
  const {
    state, output, prUrl, pendingQuestion, pendingPermission,
    run, reconnect, sendAnswer, respondToPermission, cancel, reset,
  } = useTaskRunner();
  const [lastForm, setLastForm] = useState<TaskFormValues | null>(null);

  // Sync view when user navigates with browser back/forward
  useEffect(() => {
    const handler = () => setView(viewFromHash());
    window.addEventListener("hashchange", handler);
    return () => window.removeEventListener("hashchange", handler);
  }, []);

  useEffect(() => {
    if (!token) return;
    getMe().then(setUserInfo).catch(() => {});
  }, [token]);

  if (!token) {
    return <LoginScreen onLogin={setToken} />;
  }

  const nav = (v: View) => () => {
    reset();
    setSelectedTask(null);
    sessionStorage.removeItem(SS_TASK);
    setView(v);
    pushHash(v);
  };

  const handleUserUpdate = (info: UserInfo) => setUserInfo(info);

  const handleSubmit = async (form: TaskFormValues) => {
    setSelectedTask(null);
    sessionStorage.removeItem(SS_TASK);
    setLastForm(form);
    await run(form);
    setRefreshTrigger((n) => n + 1);
  };

  const handleRetry = () => {
    if (lastForm) handleSubmit(lastForm);
  };

  const handleLogout = () => {
    localStorage.removeItem("tf_agent_token");
    sessionStorage.clear();
    setToken("");
    setUserInfo(null);
    pushHash("landing");
  };

  const handleSelectTask = (task: HistoryTask) => {
    if (task.status === "running" || task.status === "queued") {
      reset();
      setSelectedTask(null);
      sessionStorage.removeItem(SS_TASK);
      setView("form");
      pushHash("form");
      reconnect(task.id);
    } else {
      reset();
      setSelectedTask(task);
      sessionStorage.setItem(SS_TASK, task.id);
    }
  };

  const isRunning = state === "submitting" || state === "streaming";
  const isIdle = state === "idle";

  const showLanding  = isIdle && !selectedTask && view === "landing";
  const showForm     = isIdle && !selectedTask && view === "form";
  const showHistory  = isIdle && !selectedTask && view === "history";
  const showSettings = isIdle && !selectedTask && view === "settings";
  const showOutput   = !selectedTask && !isIdle;
  const showDetail   = !!selectedTask;

  return (
    <div style={{ display: "flex", height: "100%", background: "var(--bg)" }}>
      <Sidebar
        onLogout={handleLogout}
        userInfo={userInfo}
        onNewTask={nav("form")}
        onGoHome={nav("landing")}
        onHistory={nav("history")}
        onSettings={nav("settings")}
        activeView={view}
      />

      <div style={{ flex: 1, display: "flex", flexDirection: "column", minWidth: 0, overflow: "hidden" }}>
        <div className="topbar">
          <div className="topbar-title">{showOutput || showDetail ? "Task" : VIEW_TITLE[view]}</div>
          <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
            {/* The output panel already shows task status prominently while
                you're looking at it — only surface it here as an ambient
                indicator when a task is running in the background, e.g.
                while you've navigated to History or Settings. */}
            {!showOutput && isRunning && <span className="chip chip-running">Agent running</span>}
          </div>
        </div>

        <main style={{ flex: 1, overflowY: "auto", padding: "40px 48px" }}>
          <div style={{ maxWidth: showHistory ? 960 : showSettings ? "none" : showLanding ? 760 : 680, margin: "0 auto" }}>

            {showLanding && <HeroPage onNavigate={(page) => nav(page as View)()} />}

            {showForm && (
              <div className="card" style={{ animation: "slideUp .2s ease" }}>
                <div style={{ marginBottom: 20 }}>
                  <div style={{ fontSize: "var(--text-md)", fontWeight: 600, color: "var(--text)", letterSpacing: "-.01em" }}>New task</div>
                  <div style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", marginTop: 3 }}>Describe the infrastructure you want to generate.</div>
                </div>
                <TaskForm onSubmit={handleSubmit} loading={isRunning} />
              </div>
            )}

            {showHistory && <HistoryPage onSelectTask={handleSelectTask} refreshTrigger={refreshTrigger} />}

            {showSettings && <SettingsPage userInfo={userInfo} onUserUpdate={handleUserUpdate} />}

            {showOutput && (
              <OutputPanel
                output={output}
                state={state}
                prUrl={prUrl}
                pendingQuestion={pendingQuestion}
                pendingPermission={pendingPermission}
                onAnswer={sendAnswer}
                onPermissionResponse={respondToPermission}
                onRetry={lastForm ? handleRetry : undefined}
                onCancel={cancel}
              />
            )}

            {showDetail && (
              <TaskDetail
                taskId={selectedTask!.id}
                onBack={() => {
                  setSelectedTask(null);
                  sessionStorage.removeItem(SS_TASK);
                  setView("history");
                  pushHash("history");
                }}
              />
            )}

          </div>
        </main>
      </div>
    </div>
  );
}
