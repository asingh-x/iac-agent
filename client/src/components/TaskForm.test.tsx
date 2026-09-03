import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TaskForm } from "./TaskForm";
import * as api from "../lib/api";

describe("TaskForm", () => {
  beforeEach(() => {
    vi.spyOn(api, "listModels").mockResolvedValue({ provider: "test", models: [] });
  });

  it("shows a validation error and does not call onSubmit when task text is empty", async () => {
    const onSubmit = vi.fn();
    render(<TaskForm onSubmit={onSubmit} loading={false} />);
    await userEvent.click(screen.getByRole("button", { name: /run iac agent/i }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getByText(/task description is required/i)).toBeInTheDocument();
  });

  it("calls onSubmit with the form values when task text is present", async () => {
    const onSubmit = vi.fn();
    render(<TaskForm onSubmit={onSubmit} loading={false} />);
    // The task-description label isn't associated with the textarea via
    // htmlFor/id, so it has no accessible name — the placeholder is the
    // only reliable selector.
    const textarea = screen.getByPlaceholderText(/describe the infrastructure you need/i);
    await userEvent.type(textarea, "create an s3 bucket");
    await userEvent.click(screen.getByRole("button", { name: /run iac agent/i }));
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ task: "create an s3 bucket" }));
  });

  it("disables the submit button and shows the running label when loading=true", async () => {
    render(<TaskForm onSubmit={vi.fn()} loading={true} />);
    expect(await screen.findByRole("button", { name: /agent running/i })).toBeDisabled();
  });

  it("does not call onSubmit when clicking submit while loading=true", async () => {
    const onSubmit = vi.fn();
    render(<TaskForm onSubmit={onSubmit} loading={true} />);
    await userEvent.click(screen.getByRole("button", { name: /agent running/i }));
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("addRepo is a no-op past 2 rows", async () => {
    render(<TaskForm onSubmit={vi.fn()} loading={false} />);
    // Starts with a single empty repo row.
    expect(screen.getAllByPlaceholderText(/infra-modules|platform-tf/)).toHaveLength(1);

    await userEvent.click(screen.getByRole("button", { name: /\+ add repo/i }));
    expect(screen.getAllByPlaceholderText(/infra-modules|platform-tf/)).toHaveLength(2);

    // At 2 rows the "+ Add repo" button is unmounted entirely, so a user can
    // never trigger addRepo() again — this is the UI-level guarantee behind
    // the `if (comprehensionRepos.length >= 2) return;` guard, which is
    // otherwise unreachable through the rendered markup.
    expect(screen.queryByRole("button", { name: /\+ add repo/i })).not.toBeInTheDocument();
    expect(screen.getAllByPlaceholderText(/infra-modules|platform-tf/)).toHaveLength(2);
  });

  it("removeRepo always leaves at least one row", async () => {
    render(<TaskForm onSubmit={vi.fn()} loading={false} />);
    // With only one row, there is no remove ("✕") button rendered at all.
    expect(screen.queryByRole("button", { name: "✕" })).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /\+ add repo/i }));
    const removeButtons = screen.getAllByRole("button", { name: "✕" });
    expect(removeButtons).toHaveLength(2);

    await userEvent.click(removeButtons[0]);

    const remaining = screen.getAllByPlaceholderText(/infra-modules|platform-tf/);
    expect(remaining).toHaveLength(1);
    expect(remaining[0]).toHaveValue("");
    expect(screen.queryByRole("button", { name: "✕" })).not.toBeInTheDocument();
  });
});
