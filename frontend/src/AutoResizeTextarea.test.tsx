// Tests for the AutoResizeTextarea component.
import { describe, it, expect, vi } from "vitest";
import { render } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import AutoResizeTextarea from "./AutoResizeTextarea";

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

  it("is not editable when disabled", () => {
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={() => {}} disabled={true} />
    ));
    expect(getByRole("textbox")).toHaveAttribute("contenteditable", "false");
  });
});
