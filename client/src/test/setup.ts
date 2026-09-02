import "@testing-library/jest-dom/vitest";
import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
});

// jsdom has no scrollIntoView implementation at all — OutputPanel calls it
// on an autoscroll ref; without this stub, any test rendering OutputPanel
// throws "scrollIntoView is not a function".
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = vi.fn();
}

// jsdom has no Clipboard API — OutputPanel's code-block copy button calls
// navigator.clipboard.writeText. Stub it so tests can assert it was called
// without a real clipboard.
Object.assign(navigator, {
  clipboard: { writeText: vi.fn().mockResolvedValue(undefined) },
});
