// Shared native modal dialog behavior: dismissal, Escape isolation, and visual elevation.

import { onCleanup, onMount, type JSX } from "solid-js";

import styles from "./ModalDialog.module.css";

interface Props {
  children: JSX.Element;
  class?: string;
  onClose: () => void;
  dismissOnBackdrop?: boolean;
  dismissOnEscape?: boolean;
  restoreFocus?: () => void;
  "data-testid"?: string;
}

export default function ModalDialog(props: Props) {
  let dialogRef!: HTMLDialogElement;

  onMount(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const openerTaskId = opener?.closest<HTMLElement>("[data-task-id]")?.dataset.taskId;
    let focusRestoreScheduled = false;
    const restoreFocus = () => {
      if (focusRestoreScheduled) return;
      focusRestoreScheduled = true;
      requestAnimationFrame(() => {
        const currentTaskCard = openerTaskId
          ? [...document.querySelectorAll<HTMLElement>("[data-task-id]")].find((card) => card.dataset.taskId === openerTaskId)
          : undefined;
        if (props.restoreFocus) props.restoreFocus();
        else (opener?.isConnected ? opener : currentTaskCard)?.focus();
      });
    };
    const handleClose = () => {
      props.onClose();
      restoreFocus();
    };
    const handleCancel = (event: Event) => {
      event.preventDefault();
      if (props.dismissOnEscape !== false) dialogRef.close();
    };
    const handleKeydown = (event: KeyboardEvent) => {
      if (event.key === "Escape") event.stopPropagation();
    };
    const handleClick = (event: MouseEvent) => {
      if (props.dismissOnBackdrop === false) return;
      const bounds = dialogRef.getBoundingClientRect();
      const clickedInsideDialog = event.clientX >= bounds.left && event.clientX <= bounds.right
        && event.clientY >= bounds.top && event.clientY <= bounds.bottom;
      if (!clickedInsideDialog) handleClose();
    };

    dialogRef.addEventListener("close", handleClose);
    dialogRef.addEventListener("cancel", handleCancel);
    dialogRef.addEventListener("keydown", handleKeydown, true);
    dialogRef.addEventListener("click", handleClick);
    dialogRef.showModal();
    onCleanup(() => {
      restoreFocus();
      dialogRef.removeEventListener("close", handleClose);
      dialogRef.removeEventListener("cancel", handleCancel);
      dialogRef.removeEventListener("keydown", handleKeydown, true);
      dialogRef.removeEventListener("click", handleClick);
    });
  });

  return (
    <dialog ref={(el) => { dialogRef = el; }} class={`${styles.dialog} ${props.class ?? ""}`} data-testid={props["data-testid"]}>
      {props.children}
    </dialog>
  );
}
