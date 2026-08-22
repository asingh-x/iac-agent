import { useState, useCallback, useRef } from "react";
import { submitTask, streamTask, answerTask, cancelTask, respondToPermission as respondToPermissionAPI } from "../lib/api";
import type { TaskFormValues, OutputLine, SSEEvent, PendingPermission } from "../types";

function uid() {
  return Math.random().toString(36).slice(2);
}

export type RunState = "idle" | "submitting" | "streaming" | "waiting" | "done" | "error";

export function useTaskRunner() {
  const [state, setState] = useState<RunState>("idle");
  const [output, setOutput] = useState<OutputLine[]>([]);
  const [prUrl, setPrUrl] = useState<string | null>(null);
  const [taskId, setTaskId] = useState<string | null>(null);
  const [pendingQuestion, setPendingQuestion] = useState<string | null>(null);
  const [pendingPermission, setPendingPermission] = useState<PendingPermission | null>(null);
  const esRef = useRef<EventSource | null>(null);
  const assistantLineId = useRef<string | null>(null);
  const taskIdRef = useRef<string | null>(null);

  const run = useCallback(async (form: TaskFormValues) => {
    esRef.current?.close();
    setOutput([]);
    setPrUrl(null);
    setTaskId(null);
    setPendingQuestion(null);
    setPendingPermission(null);
    assistantLineId.current = null;
    taskIdRef.current = null;
    setState("submitting");

    try {
      const { task_id } = await submitTask(form);
      setTaskId(task_id);
      taskIdRef.current = task_id;
      // Show "Task created" immediately, marked done, before any agent events arrive
      setOutput([
        { id: uid(), kind: "tool_start", content: "", toolName: "task" },
        { id: uid(), kind: "tool_end", content: "", toolName: "task" },
      ]);
      setState("streaming");

      const es = streamTask(task_id);
      esRef.current = es;

      es.onmessage = (e: MessageEvent) => {
        const ev: SSEEvent = JSON.parse(e.data);

        switch (ev.type) {
          case "text": {
            const id = assistantLineId.current;
            if (!id) {
              const newId = uid();
              assistantLineId.current = newId;
              setOutput((prev) => [...prev, { id: newId, kind: "text", content: ev.text ?? "" }]);
            } else {
              setOutput((prev) =>
                prev.map((l) => l.id === id ? { ...l, content: l.content + (ev.text ?? "") } : l)
              );
            }
            break;
          }

          case "tool_start":
            assistantLineId.current = null;
            setOutput((prev) => [...prev, { id: uid(), kind: "tool_start", content: "", toolName: ev.tool }]);
            break;

          case "tool_end":
            assistantLineId.current = null;
            setOutput((prev) => [...prev, { id: uid(), kind: "tool_end", content: ev.output ?? "", toolName: ev.tool }]);
            break;

          case "status":
            assistantLineId.current = null;
            setOutput((prev) => [...prev, { id: uid(), kind: "status", content: ev.status ?? "" }]);
            break;

          case "waiting_for_input":
            assistantLineId.current = null;
            setPendingQuestion(ev.text ?? "The agent needs more information.");
            setState("waiting");
            break;

          case "permission_request":
            assistantLineId.current = null;
            setPendingPermission({ tool: ev.tool ?? "unknown tool", preview: ev.text ?? "" });
            setState("waiting");
            break;

          case "done":
            if (ev.pr_url) setPrUrl(ev.pr_url);
            es.close();
            setState("done");
            break;

          case "error":
            setOutput((prev) => [...prev, { id: uid(), kind: "error", content: ev.error ?? "Unknown error" }]);
            es.close();
            setState("error");
            break;
        }
      };

      es.onerror = () => {
        setOutput((prev) => [...prev, { id: uid(), kind: "error", content: "Connection lost." }]);
        es.close();
        setState("error");
      };
    } catch (err) {
      setOutput([{ id: uid(), kind: "error", content: err instanceof Error ? err.message : "Failed to submit" }]);
      setState("error");
    }
  }, []);

  const attachStream = useCallback((task_id: string) => {
    const es = streamTask(task_id);
    esRef.current = es;
    setTaskId(task_id);
    taskIdRef.current = task_id;

    es.onmessage = (e: MessageEvent) => {
      const ev: SSEEvent = JSON.parse(e.data);
      switch (ev.type) {
        case "text": {
          const id = assistantLineId.current;
          if (!id) {
            const newId = uid();
            assistantLineId.current = newId;
            setOutput((prev) => [...prev, { id: newId, kind: "text", content: ev.text ?? "" }]);
          } else {
            setOutput((prev) =>
              prev.map((l) => l.id === id ? { ...l, content: l.content + (ev.text ?? "") } : l)
            );
          }
          break;
        }
        case "tool_start":
          assistantLineId.current = null;
          setOutput((prev) => [...prev, { id: uid(), kind: "tool_start", content: "", toolName: ev.tool }]);
          break;
        case "tool_end":
          assistantLineId.current = null;
          setOutput((prev) => [...prev, { id: uid(), kind: "tool_end", content: ev.output ?? "", toolName: ev.tool }]);
          break;
        case "waiting_for_input":
          assistantLineId.current = null;
          setPendingQuestion(ev.text ?? "The agent needs more information.");
          setState("waiting");
          break;
        case "permission_request":
          assistantLineId.current = null;
          setPendingPermission({ tool: ev.tool ?? "unknown tool", preview: ev.text ?? "" });
          setState("waiting");
          break;
        case "done":
          if (ev.pr_url) setPrUrl(ev.pr_url);
          es.close();
          setState("done");
          break;
        case "error":
          setOutput((prev) => [...prev, { id: uid(), kind: "error", content: ev.error ?? "Unknown error" }]);
          es.close();
          setState("error");
          break;
      }
    };

    es.onerror = () => {
      es.close();
      setState("error");
    };
  }, []);

  const reconnect = useCallback((task_id: string) => {
    esRef.current?.close();
    setOutput([
      { id: uid(), kind: "tool_start", content: "", toolName: "task" },
      { id: uid(), kind: "tool_end", content: "", toolName: "task" },
    ]);
    setPrUrl(null);
    setPendingQuestion(null);
    setPendingPermission(null);
    assistantLineId.current = null;
    setState("streaming");
    attachStream(task_id);
  }, [attachStream]);

  const sendAnswer = useCallback(async (answer: string) => {
    const id = taskIdRef.current;
    if (!id || state !== "waiting") return;
    setPendingQuestion(null);
    setState("streaming");
    setOutput((prev) => [...prev, { id: uid(), kind: "status", content: `You: ${answer}` }]);
    assistantLineId.current = null;
    await answerTask(id, answer).catch(() => {});
  }, [state]);

  const respondToPermission = useCallback(async (allow: boolean) => {
    const id = taskIdRef.current;
    if (!id || state !== "waiting" || !pendingPermission) return;
    const tool = pendingPermission.tool;
    setPendingPermission(null);
    setState("streaming");
    setOutput((prev) => [
      ...prev,
      { id: uid(), kind: "status", content: allow ? `You: approved ${tool}` : `You: denied ${tool}` },
    ]);
    assistantLineId.current = null;
    await respondToPermissionAPI(id, allow).catch(() => {});
  }, [state, pendingPermission]);

  const cancel = useCallback(async () => {
    const id = taskIdRef.current;
    if (!id) return;
    await cancelTask(id).catch(() => {});
  }, []);

  const reset = useCallback(() => {
    esRef.current?.close();
    setOutput([]);
    setPrUrl(null);
    setTaskId(null);
    setPendingQuestion(null);
    setPendingPermission(null);
    setState("idle");
    assistantLineId.current = null;
    taskIdRef.current = null;
  }, []);

  return {
    state, output, prUrl, taskId, pendingQuestion, pendingPermission,
    run, reconnect, sendAnswer, respondToPermission, cancel, reset,
  };
}
