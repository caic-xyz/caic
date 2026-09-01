// Tests for the AutoResizeTextarea component.

import { describe, it, expect, vi } from "vitest";
import { render } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import AutoResizeTextarea from "./AutoResizeTextarea";

function selectionOffset(root: HTMLElement): number | undefined {
  const selection = window.getSelection();
  const anchor = selection?.anchorNode;
  if (!anchor || !root.contains(anchor)) return undefined;
  let offset = 0;
  const visit = (node: Node): number | undefined => {
    if (node === anchor) {
      if (node.nodeType === Node.TEXT_NODE) return offset + (selection?.anchorOffset ?? 0);
      for (const child of [...node.childNodes].slice(0, selection?.anchorOffset ?? 0)) {
        offset += child.textContent?.length ?? 0;
      }
      return offset;
    }
    for (const child of node.childNodes) {
      if (child === anchor || child.contains(anchor)) {
        const found = visit(child);
        if (found !== undefined) return found;
      } else {
        offset += child.textContent?.length ?? 0;
      }
    }
    return undefined;
  };
  return visit(root);
}

describe("AutoResizeTextarea", () => {
  it("renders with placeholder", () => {
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={() => {}} placeholder="Type here" />
    ));
    expect(getByRole("textbox")).toHaveAttribute("data-placeholder", "Type here");
  });

  it("calls onInput when typing", async () => {
    const user = userEvent.setup();
    const onInput = vi.fn();
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={onInput} />
    ));
    await user.click(getByRole("textbox"));
    await user.keyboard("a");
    expect(onInput).toHaveBeenCalledWith("a");
  });

  it("calls onSubmit on Enter", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={() => {}} onSubmit={onSubmit} />
    ));
    getByRole("textbox").focus();
    await user.keyboard("{Enter}");
    expect(onSubmit).toHaveBeenCalledOnce();
  });

  it("does not call onSubmit on Shift+Enter", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={() => {}} onSubmit={onSubmit} />
    ));
    getByRole("textbox").focus();
    await user.keyboard("{Shift>}{Enter}{/Shift}");
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("preserves newlines when value is restored", () => {
    const { getByRole } = render(() => (
      <AutoResizeTextarea value={"line1\nline2\nline3"} onInput={() => {}} />
    ));
    const el = getByRole("textbox");
    // The contentEditable should contain <br> elements for newlines.
    const brs = el.querySelectorAll("br");
    expect(brs.length).toBe(2);
    // getText should round-trip back to the original value.
    expect(el.textContent?.replace(/\n/g, "")).toBe("line1line2line3");
    // Verify the DOM structure preserves newlines by checking innerHTML.
    expect(el.innerHTML).toContain("line1");
    expect(el.innerHTML).toContain("line2");
    expect(el.innerHTML).toContain("<br>");
  });

  it("extracts newlines from div-wrapped lines", async () => {
    const onInput = vi.fn();
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={onInput} />
    ));
    const el = getByRole("textbox");
    // Simulate Chrome's contentEditable behaviour: wrapping lines in <div>.
    el.innerHTML = "line1<div>line2</div><div>line3</div>";
    el.dispatchEvent(new Event("input", { bubbles: true }));
    expect(onInput).toHaveBeenCalledWith("line1\nline2\nline3");
  });

  it("places the caret at the end on programmatic focus", async () => {
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="hello" onInput={() => {}} />
    ));
    const el = getByRole("textbox");

    el.focus();
    await Promise.resolve();

    expect(selectionOffset(el)).toBe(5);
  });

  it("restores the last caret position on keyboard focus", async () => {
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="hello" onInput={() => {}} />
    ));
    const el = getByRole("textbox");
    el.focus();
    await Promise.resolve();
    const text = el.firstChild;
    if (!text) throw new Error("Editable text was not rendered");
    const range = document.createRange();
    range.setStart(text, 2);
    range.collapse(true);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    document.dispatchEvent(new Event("selectionchange"));
    el.blur();

    el.focus();
    await Promise.resolve();

    expect(selectionOffset(el)).toBe(2);
  });

  it("is not editable when disabled", () => {
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={() => {}} disabled={true} />
    ));
    expect(getByRole("textbox")).toHaveAttribute("contenteditable", "false");
  });
});
