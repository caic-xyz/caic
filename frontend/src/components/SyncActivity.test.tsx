// Tests the push entry in running, succeeded, failed, and cancelled states, its cancel control, and log following.

import { it } from "node:test";
import { fireEvent, render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { createSignal } from "solid-js";
import { expect, vi } from "@tests/expect";

import type { SyncEntry } from "../syncActivity";
import SyncActivity from "./SyncActivity";

const running: SyncEntry = {
  id: "a",
  branch: "feature",
  target: "origin",
  output: "",
  status: null,
  error: null,
  durationMs: null,
  anchorTs: Date.now() - 4_000,
};

it("shows a running push with its branch and a cancel button", async () => {
  const onCancel = vi.fn(() => Promise.resolve(true));
  render(() => <SyncActivity entry={running} onCancel={onCancel} />);
  expect(screen.getByRole("group", { name: "Push to feature" })).toHaveAttribute("data-state", "running");
  expect(screen.getByRole("status")).toHaveTextContent("Running");
  const cancel = screen.getByRole("button", { name: "Cancel push" });
  await userEvent.setup().click(cancel);
  expect(onCancel).toHaveBeenCalledOnce();
  expect(cancel).toBeDisabled();
});

it("lets a refused cancel be retried", async () => {
  render(() => <SyncActivity entry={running} onCancel={() => Promise.resolve(false)} />);
  const cancel = screen.getByRole("button", { name: "Cancel push" });
  await userEvent.setup().click(cancel);
  await vi.waitFor(() => expect(cancel).toBeEnabled());
});

it("shows a succeeded push with its duration and no cancel button", () => {
  render(() => (
    <SyncActivity
      entry={{ ...running, status: "pushed", durationMs: 65_000, output: "hook ok\n" }}
      onCancel={() => Promise.resolve(true)}
    />
  ));
  expect(screen.getByTestId("sync-activity")).toHaveAttribute("data-state", "pushed");
  expect(screen.getByRole("status")).toHaveTextContent("Pushed");
  expect(screen.getByTestId("sync-duration")).toHaveTextContent("1:05");
  expect(screen.getByTestId("sync-output")).toHaveTextContent("hook ok");
  expect(screen.queryByRole("button", { name: "Cancel push" })).not.toBeInTheDocument();
});

it("keeps a failed push and its error visible", () => {
  render(() => (
    <SyncActivity
      entry={{ ...running, status: "failed", error: "pre-push hook failed", durationMs: 2_000 }}
      onCancel={() => Promise.resolve(true)}
    />
  ));
  expect(screen.getByRole("status")).toHaveTextContent("Failed");
  expect(screen.getByTestId("sync-error")).toHaveTextContent("pre-push hook failed");
  expect(screen.queryByRole("button", { name: "Cancel push" })).not.toBeInTheDocument();
});

it("shows a cancelled push without a failure message", () => {
  render(() => (
    <SyncActivity
      entry={{ ...running, status: "cancelled", error: "sync cancelled", durationMs: 2_000 }}
      onCancel={() => Promise.resolve(true)}
    />
  ));
  expect(screen.getByRole("status")).toHaveTextContent("Cancelled");
  expect(screen.getByTestId("sync-activity")).toHaveAttribute("data-state", "cancelled");
  expect(screen.queryByTestId("sync-error")).not.toBeInTheDocument();
});

it("shows confirmed updates when a push is cancelled partway", () => {
  const error = 'pre-push hook for "/repo" failed after Git confirmed updates to "/repo/lib"';
  render(() => <SyncActivity entry={{ ...running, status: "cancelled", error, durationMs: 2_000 }} onCancel={null} />);
  expect(screen.getByRole("status")).toHaveTextContent("Cancelled");
  expect(screen.getByTestId("sync-error")).toHaveTextContent(error);
});

it("offers no cancel for an entry outside the live push", () => {
  render(() => <SyncActivity entry={running} onCancel={null} />);
  expect(screen.queryByRole("button", { name: "Cancel push" })).not.toBeInTheDocument();
});

function scrollMetrics(log: HTMLElement) {
  Object.defineProperty(log, "scrollHeight", { configurable: true, get: () => 1000 });
  Object.defineProperty(log, "clientHeight", { configurable: true, get: () => 100 });
}

it("follows new output until the reader scrolls away, and resumes at the bottom", () => {
  const [entry, setEntry] = createSignal<SyncEntry>({ ...running, output: "one\n" });
  render(() => <SyncActivity entry={entry()} onCancel={() => Promise.resolve(true)} />);
  const log = screen.getByTestId("sync-output");
  scrollMetrics(log);
  setEntry((previous) => ({ ...previous, output: "one\ntwo\n" }));
  expect(log.scrollTop).toBe(1000);
  log.scrollTop = 200;
  fireEvent.scroll(log);
  setEntry((previous) => ({ ...previous, output: "one\ntwo\nthree\n" }));
  expect(log.scrollTop).toBe(200);
  log.scrollTop = 900;
  fireEvent.scroll(log);
  setEntry((previous) => ({ ...previous, output: "one\ntwo\nthree\nfour\n" }));
  expect(log.scrollTop).toBe(1000);
});

it("stops following once the reader selects log text", () => {
  const [entry, setEntry] = createSignal<SyncEntry>({ ...running, output: "one\n" });
  render(() => <SyncActivity entry={entry()} onCancel={() => Promise.resolve(true)} />);
  const log = screen.getByTestId("sync-output");
  scrollMetrics(log);
  const selection = document.getSelection();
  selection?.selectAllChildren(log);
  fireEvent(document, new Event("selectionchange"));
  log.scrollTop = 0;
  setEntry((previous) => ({ ...previous, output: "one\ntwo\n" }));
  expect(log.scrollTop).toBe(0);
  selection?.removeAllRanges();
});
