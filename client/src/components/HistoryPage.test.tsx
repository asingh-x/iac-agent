import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, act, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HistoryPage } from "./HistoryPage";
import * as api from "../lib/api";
import type { TaskDetail } from "../types";

const sampleTask = (overrides: Partial<TaskDetail> = {}): TaskDetail => ({
  id: "t1",
  status: "done",
  input_text: "create an s3 bucket",
  created_at: new Date().toISOString(),
  output_type: "text",
  input_tokens: 0,
  output_tokens: 0,
  ...overrides,
});

describe("HistoryPage", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("shows a loading state, then the empty state when there are no tasks", async () => {
    vi.spyOn(api, "listTasks").mockResolvedValue([]);
    render(<HistoryPage onSelectTask={vi.fn()} refreshTrigger={0} />);
    // Loading is the synchronous initial state, before the listTasks() promise settles.
    expect(screen.getByText(/loading/i)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(/no tasks yet/i)).toBeInTheDocument());
  });

  it("renders a row per task and re-fetches when refreshTrigger changes", async () => {
    const listTasks = vi.spyOn(api, "listTasks").mockResolvedValue([sampleTask()]);
    const { rerender } = render(<HistoryPage onSelectTask={vi.fn()} refreshTrigger={0} />);
    await waitFor(() => expect(screen.getByText(/create an s3 bucket/i)).toBeInTheDocument());
    expect(listTasks).toHaveBeenCalledTimes(1);

    rerender(<HistoryPage onSelectTask={vi.fn()} refreshTrigger={1} />);
    await waitFor(() => expect(listTasks).toHaveBeenCalledTimes(2));
  });

  it("expands a row on click and calls onSelectTask from 'Full detail'", async () => {
    vi.spyOn(api, "listTasks").mockResolvedValue([sampleTask()]);
    const onSelectTask = vi.fn();
    render(<HistoryPage onSelectTask={onSelectTask} refreshTrigger={0} />);
    await waitFor(() => screen.getByText(/create an s3 bucket/i));
    await userEvent.click(screen.getByText(/create an s3 bucket/i));
    await userEvent.click(screen.getByRole("button", { name: /full detail/i }));
    expect(onSelectTask).toHaveBeenCalledWith(expect.objectContaining({ id: "t1" }));
  });

  it("shows Cancel only for cancellable statuses (waiting_for_input|running|queued)", async () => {
    vi.spyOn(api, "listTasks").mockResolvedValue([sampleTask({ status: "done" })]);
    render(<HistoryPage onSelectTask={vi.fn()} refreshTrigger={0} />);
    await waitFor(() => screen.getByText(/create an s3 bucket/i));
    await userEvent.click(screen.getByText(/create an s3 bucket/i));
    expect(screen.queryByRole("button", { name: /cancel/i })).not.toBeInTheDocument();
  });

  it("cancel button calls cancelTask and polls listTasks afterward (fake timers)", async () => {
    const listTasks = vi
      .spyOn(api, "listTasks")
      .mockResolvedValueOnce([sampleTask({ status: "running" })])
      .mockResolvedValue([sampleTask({ status: "cancelled" })]);
    const cancelTask = vi.spyOn(api, "cancelTask").mockResolvedValue(undefined);

    // Let the initial fetch resolve under real timers first — only the
    // component's own recursive setTimeout(..., 600) polling needs faking.
    render(<HistoryPage onSelectTask={vi.fn()} refreshTrigger={0} />);
    await waitFor(() => expect(listTasks).toHaveBeenCalledTimes(1));
    await screen.findByText(/create an s3 bucket/i);

    vi.useFakeTimers();
    try {
      // Plain fireEvent (synchronous, no internal setTimeout-based pointer
      // sequencing) instead of userEvent here — userEvent's own internals
      // schedule via setTimeout, which hangs once real timers are faked.
      fireEvent.click(screen.getByText(/create an s3 bucket/i));
      // Real button text is "⏹ Cancel" (leading icon glyph), so match loosely.
      fireEvent.click(screen.getByRole("button", { name: /cancel/i }));
      expect(cancelTask).toHaveBeenCalledWith("t1");

      // Advance fake time (wrapped in act, since this drives a React state
      // update outside of any RTL-wrapped interaction) instead of waiting
      // on real wall-clock time.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(600);
      });
      expect(listTasks).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("pagination: changing page size resets to page 1", async () => {
    const tasks = Array.from({ length: 15 }, (_, i) => sampleTask({ id: `t${i}`, input_text: `task ${i}` }));
    vi.spyOn(api, "listTasks").mockResolvedValue(tasks);
    render(<HistoryPage onSelectTask={vi.fn()} refreshTrigger={0} />);
    await waitFor(() => screen.getByText(/task 0/i));

    // The prev/next controls are plain buttons whose accessible name is
    // their arrow glyph ("←" / "→"), not the word "Next".
    await userEvent.click(screen.getByRole("button", { name: "→" }));
    // Now on page 2 with default page size 10 — confirm task 0 no longer shown.
    expect(screen.queryByText(/^task 0$/i)).not.toBeInTheDocument();

    // Changing page size should reset back to page 1.
    await userEvent.selectOptions(screen.getByRole("combobox"), "25");
    expect(screen.getByText(/^task 0$/i)).toBeInTheDocument();
  });
});
