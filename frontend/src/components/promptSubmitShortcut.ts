// Handles Ctrl+Enter submission within a prompt form without activating its focused control.

import { onCleanup } from "solid-js";

export function bindPromptSubmitShortcut(form: HTMLFormElement) {
  const onKeyDown = (event: KeyboardEvent) => {
    if (event.key !== "Enter" || !event.ctrlKey || event.shiftKey || event.altKey || event.metaKey) return;
    // CameraCapture is nested inside PromptInput. Its modal owns keyboard input
    // while open, even though the dialog remains a descendant of this form.
    if (form.ownerDocument.querySelector("dialog[open]")) return;
    event.preventDefault();
    event.stopPropagation();
    if (event.repeat) return;

    const submitButton = form.querySelector<HTMLButtonElement>('button[type="submit"]');
    if (submitButton && !submitButton.disabled) form.requestSubmit(submitButton);
  };
  form.addEventListener("keydown", onKeyDown, true);
  onCleanup(() => form.removeEventListener("keydown", onKeyDown, true));
}
