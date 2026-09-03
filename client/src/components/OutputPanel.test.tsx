import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { OutputPanel } from "./OutputPanel";
import type { OutputLine } from "../types";

describe("OutputPanel", () => {
  it("pairs a tool_start/tool_end into one collapsible block", async () => {
    const output: OutputLine[] = [
      { id: "1", kind: "tool_start", toolName: "validate_terraform", content: "" },
      { id: "2", kind: "tool_end", toolName: "validate_terraform", content: "Success! The configuration is valid." },
    ];
    render(<OutputPanel output={output} state="done" prUrl={null} />);

    // friendlyLabel("validate_terraform") -> "Validating" — a single block,
    // not two, since tool_end mutates the existing tool_start item rather
    // than pushing a new one.
    const toolButton = screen.getByRole("button", { name: /validating/i });
    expect(toolButton).toBeInTheDocument();

    // The tool_end output isn't shown until expanded, and only appears at
    // all because tool_end was correctly paired (by toolName) with its
    // tool_start — this is the real evidence of pairing, not just the label.
    expect(screen.queryByText("Success! The configuration is valid.")).not.toBeInTheDocument();
    await userEvent.click(toolButton);
    expect(screen.getByText("Success! The configuration is valid.")).toBeInTheDocument();
  });

  it("shows the PR banner only when prUrl is set", () => {
    const { rerender } = render(<OutputPanel output={[]} state="done" prUrl={null} />);
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    rerender(<OutputPanel output={[]} state="done" prUrl="https://github.com/org/repo/pull/1" />);
    const link = screen.getByRole("link");
    expect(link).toHaveAttribute("href", "https://github.com/org/repo/pull/1");
  });

  it("shows the waiting-for-input callout only when state=waiting, and submits via Ctrl+Enter", async () => {
    const onAnswer = vi.fn();
    const { rerender } = render(
      <OutputPanel output={[]} state="done" prUrl={null} pendingQuestion="Which region?" onAnswer={onAnswer} />
    );
    // pendingQuestion alone isn't enough — the callout requires state="waiting" too.
    expect(screen.queryByText("Which region?")).not.toBeInTheDocument();

    rerender(
      <OutputPanel output={[]} state="waiting" prUrl={null} pendingQuestion="Which region?" onAnswer={onAnswer} />
    );
    expect(screen.getByText("Which region?")).toBeInTheDocument();

    const textarea = screen.getByRole("textbox");
    await userEvent.type(textarea, "us-east-1");
    // The textarea's onKeyDown wires Cmd/Ctrl+Enter to submitAnswer().
    await userEvent.keyboard("{Control>}{Enter}{/Control}");
    expect(onAnswer).toHaveBeenCalledWith("us-east-1");
  });

  it("does not call onAnswer and shows a validation error when the answer is empty or whitespace-only", async () => {
    const onAnswer = vi.fn();
    render(
      <OutputPanel output={[]} state="waiting" prUrl={null} pendingQuestion="Which region?" onAnswer={onAnswer} />
    );
    const sendButton = screen.getByRole("button", { name: /send/i });

    await userEvent.click(sendButton);
    expect(onAnswer).not.toHaveBeenCalled();
    expect(screen.getByText("Please enter an answer.")).toBeInTheDocument();

    await userEvent.type(screen.getByRole("textbox"), "   ");
    await userEvent.click(sendButton);
    expect(onAnswer).not.toHaveBeenCalled();
  });

  it("shows the permission callout and calls onPermissionResponse(true/false) for Approve/Deny", async () => {
    const onPermissionResponse = vi.fn();
    render(
      <OutputPanel
        output={[]}
        state="waiting"
        prUrl={null}
        pendingPermission={{ tool: "bash", preview: "echo hi" }}
        onPermissionResponse={onPermissionResponse}
      />
    );
    expect(screen.getByText(/approve tool call: bash/i)).toBeInTheDocument();
    expect(screen.getByText("echo hi")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /^approve$/i }));
    expect(onPermissionResponse).toHaveBeenCalledWith(true);

    await userEvent.click(screen.getByRole("button", { name: /^deny$/i }));
    expect(onPermissionResponse).toHaveBeenCalledWith(false);
  });

  it("shows Stop while running and Retry once terminal, wired to onCancel/onRetry", async () => {
    const onCancel = vi.fn();
    const onRetry = vi.fn();
    const { rerender } = render(
      <OutputPanel output={[]} state="streaming" prUrl={null} onCancel={onCancel} onRetry={onRetry} />
    );
    const stopButton = screen.getByRole("button", { name: /stop/i });
    expect(screen.queryByRole("button", { name: /retry/i })).not.toBeInTheDocument();
    await userEvent.click(stopButton);
    expect(onCancel).toHaveBeenCalled();

    // Retry only appears for terminal states (done/error) — Stop unmounts
    // entirely rather than merely disabling.
    rerender(<OutputPanel output={[]} state="done" prUrl={null} onCancel={onCancel} onRetry={onRetry} />);
    const retryButton = screen.getByRole("button", { name: /retry/i });
    expect(screen.queryByRole("button", { name: /stop/i })).not.toBeInTheDocument();
    await userEvent.click(retryButton);
    expect(onRetry).toHaveBeenCalled();
  });

  it("copies a fenced code block's content via the copy button", async () => {
    const output: OutputLine[] = [
      {
        id: "1",
        kind: "text",
        content: '```hcl\nresource "aws_s3_bucket" "example" {}\n```',
      },
    ];
    render(<OutputPanel output={output} state="done" prUrl={null} />);

    await userEvent.click(screen.getByRole("button", { name: /copy/i }));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith('resource "aws_s3_bucket" "example" {}\n');

    // Wait for the copy button's own state update (from the writeText
    // promise's .then()) to settle before the test ends, so it doesn't
    // land outside of act().
    await screen.findByRole("button", { name: "✓" });
  });
});
