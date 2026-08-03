// Tests for the shared native modal dialog's dismissal and Escape behavior.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@solidjs/testing-library";

import ModalDialog from "./ModalDialog";

function renderDialog(props: Partial<{
  dismissOnBackdrop: boolean;
  dismissOnEscape: boolean;
}> = {}) {
  const onClose = vi.fn();
  render(() => (
    <ModalDialog {...props} onClose={onClose} data-testid="modal-dialog">
      <p>Dialog content</p>
    </ModalDialog>
  ));
  return { dialog: screen.getByTestId("modal-dialog") as HTMLDialogElement, onClose };
}

describe("ModalDialog", () => {
  it("dismisses when its backdrop is clicked, but not when the dialog is clicked", () => {
    const { dialog, onClose } = renderDialog();
    vi.spyOn(dialog, "getBoundingClientRect").mockReturnValue(new DOMRect(100, 100, 400, 300));

    fireEvent.click(dialog, { clientX: 150, clientY: 150 });
    expect(onClose).not.toHaveBeenCalled();

    fireEvent.click(dialog, { clientX: 50, clientY: 50 });
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("isolates Escape from page handlers and closes through the native close event", () => {
    const { dialog, onClose } = renderDialog();
    const onKeyDown = vi.fn();
    document.addEventListener("keydown", onKeyDown);

    fireEvent.keyDown(dialog, { key: "Escape" });
    document.removeEventListener("keydown", onKeyDown);
    expect(onKeyDown).not.toHaveBeenCalled();

    dialog.close();
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("can prevent Escape dismissal", () => {
    const { dialog, onClose } = renderDialog({ dismissOnEscape: false });
    const cancel = new Event("cancel", { cancelable: true });

    dialog.dispatchEvent(cancel);

    expect(cancel.defaultPrevented).toBe(true);
    expect(onClose).not.toHaveBeenCalled();
  });
});
