// Tests for the shared native modal dialog's dismissal and Escape behavior.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { createSignal, Show } from "solid-js";

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

  it("restores focus to the opener after closing", async () => {
    const user = userEvent.setup();
    function DialogHost() {
      const [open, setOpen] = createSignal(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>Open dialog</button>
          <Show when={open()}>
            <ModalDialog onClose={() => setOpen(false)} data-testid="modal-dialog">
              <button type="button">Inside dialog</button>
            </ModalDialog>
          </Show>
        </>
      );
    }
    render(() => <DialogHost />);
    const opener = screen.getByRole("button", { name: "Open dialog" });

    await user.click(opener);
    (screen.getByTestId("modal-dialog") as HTMLDialogElement).close();

    await waitFor(() => expect(opener).toHaveFocus());
  });

  it("restores focus to a task card replaced while the dialog is open", async () => {
    const user = userEvent.setup();
    function DialogHost() {
      const [open, setOpen] = createSignal(false);
      return (
        <>
          <button type="button" data-task-id="task-1" onClick={() => setOpen(true)}>Open task dialog</button>
          <Show when={open()}>
            <ModalDialog onClose={() => setOpen(false)} data-testid="modal-dialog">
              <p>Dialog content</p>
            </ModalDialog>
          </Show>
        </>
      );
    }
    render(() => <DialogHost />);
    const opener = screen.getByRole("button", { name: "Open task dialog" });

    await user.click(opener);
    const replacement = opener.cloneNode(true) as HTMLButtonElement;
    opener.replaceWith(replacement);
    (screen.getByTestId("modal-dialog") as HTMLDialogElement).close();

    await waitFor(() => expect(replacement).toHaveFocus());
  });

  it("keeps focus where it moved while the restore frame was pending", async () => {
    const user = userEvent.setup();
    function DialogHost() {
      const [open, setOpen] = createSignal(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>Open dialog</button>
          <button type="button">Elsewhere</button>
          <Show when={open()}>
            <ModalDialog onClose={() => setOpen(false)} data-testid="modal-dialog">
              <p>Dialog content</p>
            </ModalDialog>
          </Show>
        </>
      );
    }
    render(() => <DialogHost />);
    const opener = screen.getByRole("button", { name: "Open dialog" });
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });

    await user.click(opener);
    (screen.getByTestId("modal-dialog") as HTMLDialogElement).close();
    elsewhere.focus();

    await waitFor(() => expect(screen.queryByTestId("modal-dialog")).not.toBeInTheDocument());
    await new Promise((resolve) => { requestAnimationFrame(resolve); });
    expect(elsewhere).toHaveFocus();
  });

  it("closes explicitly after an allowed native cancel", () => {
    const { dialog, onClose } = renderDialog();
    const cancel = new Event("cancel", { cancelable: true });

    dialog.dispatchEvent(cancel);

    expect(cancel.defaultPrevented).toBe(true);
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
