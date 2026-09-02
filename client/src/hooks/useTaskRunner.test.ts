import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { useTaskRunner } from "./useTaskRunner";
import * as api from "../lib/api";
import type { TaskFormValues } from "../types";

// A minimal fake EventSource the test drives manually — real EventSource
// isn't implemented in jsdom, and streamTask() constructs one directly.
class FakeEventSource {
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  close = vi.fn();
  emit(data: object) {
    this.onmessage?.({ data: JSON.stringify(data) } as MessageEvent);
  }
}

function form(overrides: Partial<TaskFormValues> = {}): TaskFormValues {
  return {
    task: "do a thing",
    jiraTicket: "",
    workRepo: "",
    comprehensionRepos: [""],
    dryRun: false,
    model: "",
    ...overrides,
  };
}

describe("useTaskRunner", () => {
  let fakeES: FakeEventSource;

  beforeEach(() => {
    vi.restoreAllMocks();
    fakeES = new FakeEventSource();
    vi.spyOn(api, "streamTask").mockReturnValue(fakeES as unknown as EventSource);
    vi.spyOn(api, "submitTask").mockResolvedValue({ task_id: "task-1" });
    vi.spyOn(api, "answerTask").mockResolvedValue(undefined);
    vi.spyOn(api, "respondToPermission").mockResolvedValue(undefined);
  });

  it("starts idle", () => {
    const { result } = renderHook(() => useTaskRunner());
    expect(result.current.state).toBe("idle");
  });

  it("run() submits, then transitions to streaming and opens a stream", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    expect(result.current.state).toBe("streaming");
    expect(result.current.taskId).toBe("task-1");
    expect(api.streamTask).toHaveBeenCalledWith("task-1");
  });

  it("a 'done' SSE event sets state=done and captures pr_url", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    act(() => {
      fakeES.emit({ type: "done", pr_url: "https://github.com/org/repo/pull/1" });
    });
    await waitFor(() => expect(result.current.state).toBe("done"));
    expect(result.current.prUrl).toBe("https://github.com/org/repo/pull/1");
    expect(fakeES.close).toHaveBeenCalled();
  });

  it("a 'waiting_for_input' SSE event sets state=waiting and pendingQuestion", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    act(() => {
      fakeES.emit({ type: "waiting_for_input", text: "Which region?" });
    });
    await waitFor(() => expect(result.current.state).toBe("waiting"));
    expect(result.current.pendingQuestion).toBe("Which region?");
  });

  it("submitTask rejection sets state=error with an error output line", async () => {
    vi.spyOn(api, "submitTask").mockRejectedValue(new Error("boom"));
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    expect(result.current.state).toBe("error");
    expect(result.current.output).toHaveLength(1);
    expect(result.current.output[0]).toMatchObject({ kind: "error", content: "boom" });
  });

  it("es.onerror sets state=error and closes the stream", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    act(() => {
      fakeES.onerror?.();
    });
    await waitFor(() => expect(result.current.state).toBe("error"));
    expect(fakeES.close).toHaveBeenCalled();
  });

  it("reset() returns to idle and clears output/prUrl/taskId", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    act(() => result.current.reset());
    expect(result.current.state).toBe("idle");
    expect(result.current.taskId).toBeNull();
    expect(result.current.prUrl).toBeNull();
    expect(result.current.output).toHaveLength(0);
  });

  it("sendAnswer is a no-op when state !== 'waiting'", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    expect(result.current.state).toBe("streaming");

    await act(async () => {
      await result.current.sendAnswer("us-east-1");
    });

    expect(api.answerTask).not.toHaveBeenCalled();
    expect(result.current.state).toBe("streaming");
  });

  it("sendAnswer calls answerTask and moves back toward 'streaming' when state is 'waiting'", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    act(() => {
      fakeES.emit({ type: "waiting_for_input", text: "Which region?" });
    });
    await waitFor(() => expect(result.current.state).toBe("waiting"));

    await act(async () => {
      await result.current.sendAnswer("us-east-1");
    });

    expect(api.answerTask).toHaveBeenCalledWith("task-1", "us-east-1");
    expect(result.current.state).toBe("streaming");
    expect(result.current.pendingQuestion).toBeNull();
  });

  it("respondToPermission is a no-op when state !== 'waiting'", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    expect(result.current.state).toBe("streaming");

    await act(async () => {
      await result.current.respondToPermission(true);
    });

    expect(api.respondToPermission).not.toHaveBeenCalled();
    expect(result.current.state).toBe("streaming");
  });

  it("respondToPermission calls respondToPermission and moves back toward 'streaming' when state is 'waiting'", async () => {
    const { result } = renderHook(() => useTaskRunner());
    await act(async () => {
      await result.current.run(form());
    });
    act(() => {
      fakeES.emit({ type: "permission_request", tool: "bash", text: "rm -rf /tmp/x" });
    });
    await waitFor(() => expect(result.current.state).toBe("waiting"));
    expect(result.current.pendingPermission).toEqual({ tool: "bash", preview: "rm -rf /tmp/x" });

    await act(async () => {
      await result.current.respondToPermission(true);
    });

    expect(api.respondToPermission).toHaveBeenCalledWith("task-1", true);
    expect(result.current.state).toBe("streaming");
    expect(result.current.pendingPermission).toBeNull();
  });
});
