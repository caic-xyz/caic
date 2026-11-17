// Tests waiting for lazily rendered elements, cancellation on user scrolling, and revealing one with focus and a mark.

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { abortOnUserScroll, revealElement, waitForElement } from "./revealElement";

describe("waitForElement", () => {
  it("resolves when the element renders, and null on abort", async () => {
    const root = document.createElement("div");
    document.body.append(root);
    const find = () => root.querySelector<HTMLElement>("#late");
    const pending = waitForElement(root, find, new AbortController().signal, 1_000);
    const late = document.createElement("p");
    late.id = "late";
    root.append(late);
    assert.equal(await pending, late);
    const abort = new AbortController();
    const aborted = waitForElement(root, () => root.querySelector<HTMLElement>("#never"), abort.signal, 1_000);
    abort.abort();
    assert.equal(await aborted, null);
    root.remove();
  });
});

describe("revealElement", () => {
  it("opens enclosing disclosures, focuses the summary, and moves a brief mark to the latest element", () => {
    vi.useFakeTimers();
    try {
      document.body.innerHTML =
        "<details id='group'><summary>group</summary><details id='call'><summary>call</summary></details></details>";
      const call = document.getElementById("call") as HTMLDetailsElement;
      const group = document.getElementById("group") as HTMLDetailsElement;
      revealElement(group, "mark");
      revealElement(call, "mark");
      expect(group.classList.contains("mark")).toBe(false);
      expect((document.getElementById("group") as HTMLDetailsElement).open).toBe(true);
      expect(call.open).toBe(false);
      expect(document.activeElement).toBe(call.querySelector("summary"));
      expect(call.classList.contains("mark")).toBe(true);
      vi.advanceTimersByTime(2_000);
      expect(call.classList.contains("mark")).toBe(false);
    } finally {
      vi.useRealTimers();
      document.body.innerHTML = "";
    }
  });

  it("focuses a row without a control through a programmatic tab stop", () => {
    document.body.innerHTML = "<div id='row'><p>text</p></div>";
    const row = document.getElementById("row") as HTMLElement;
    revealElement(row, "mark");
    expect(row.tabIndex).toBe(-1);
    expect(document.activeElement).toBe(row);
    document.body.innerHTML = "";
  });
});

// jsdom's addEventListener accepts only its own AbortSignal.
describe("abortOnUserScroll", () => {
  it("aborts on a wheel or a scroll key, ignores other keys, and stops listening once aborted", () => {
    const wheel = new window.AbortController();
    abortOnUserScroll(wheel);
    window.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Tab" }));
    expect(wheel.signal.aborted).toBe(false);
    window.dispatchEvent(new window.Event("wheel"));
    expect(wheel.signal.aborted).toBe(true);
    const key = new window.AbortController();
    abortOnUserScroll(key);
    window.dispatchEvent(new window.KeyboardEvent("keydown", { key: "PageDown" }));
    expect(key.signal.aborted).toBe(true);
  });

  it("cancels a pending wait when the user scrolls", async () => {
    const abort = new window.AbortController();
    abortOnUserScroll(abort);
    const pending = waitForElement(document.body, () => null, abort.signal, 10_000);
    window.dispatchEvent(new window.Event("touchmove"));
    assert.equal(await pending, null);
  });
});
