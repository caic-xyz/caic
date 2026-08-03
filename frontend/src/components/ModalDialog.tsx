// Shared native modal dialog behavior: dismissal, Escape isolation, and visual elevation.

import { onCleanup, onMount, type JSX } from "solid-js";

import styles from "./ModalDialog.module.css";

interface Props {
  children: JSX.Element;
  class?: string;
  onClose: () => void;
  dismissOnBackdrop?: boolean;
  dismissOnEscape?: boolean;
  "data-testid"?: string;
}

export default function ModalDialog(props: Props) {
  let dialogRef!: HTMLDialogElement;

  onMount(() => {
    const handleClose = () => props.onClose();
    const handleCancel = (event: Event) => {
      if (props.dismissOnEscape === false) event.preventDefault();
    };
    const handleKeydown = (event: KeyboardEvent) => {
      if (event.key === "Escape") event.stopPropagation();
    };
    const handleClick = (event: MouseEvent) => {
      if (props.dismissOnBackdrop === false) return;
      const bounds = dialogRef.getBoundingClientRect();
      const clickedInsideDialog = event.clientX >= bounds.left && event.clientX <= bounds.right
        && event.clientY >= bounds.top && event.clientY <= bounds.bottom;
      if (!clickedInsideDialog) props.onClose();
    };

    dialogRef.addEventListener("close", handleClose);
    dialogRef.addEventListener("cancel", handleCancel);
    dialogRef.addEventListener("keydown", handleKeydown, true);
    dialogRef.addEventListener("click", handleClick);
    dialogRef.showModal();
    onCleanup(() => {
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
