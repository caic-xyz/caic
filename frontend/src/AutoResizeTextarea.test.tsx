// Tests for the AutoResizeTextarea component.
import { describe, it, expect, vi } from "vitest";
import { render } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import AutoResizeTextarea from "./AutoResizeTextarea";

describe("AutoResizeTextarea", () => {
  it("renders with placeholder", () => {
    const { getByPlaceholderText } = render(() => (
      <AutoResizeTextarea value="" onInput={() => {}} placeholder="Type here" />
    ));
    expect(getByPlaceholderText("Type here")).toBeInTheDocument();
  });

  it("calls onInput when typing", async () => {
    const user = userEvent.setup();
    const onInput = vi.fn();
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={onInput} />
    ));
    await user.type(getByRole("textbox"), "a");
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

  it("renders as disabled when disabled prop is set", () => {
    const { getByRole } = render(() => (
      <AutoResizeTextarea value="" onInput={() => {}} disabled={true} />
    ));
    expect(getByRole("textbox")).toBeDisabled();
  });
});
