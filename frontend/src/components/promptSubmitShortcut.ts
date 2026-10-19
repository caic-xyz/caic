// Handles Ctrl+Enter prompt submission or steering without activating the focused control.

import { onCleanup } from "solid-js";

export function bindPromptSubmitShortcut(form: HTMLFormElement, onSubmit: () => void) {
  const onKeyDown = (event: KeyboardEvent) => {
    if (event.isComposing || event.key !== "Enter" || !event.ctrlKey || event.shiftKey || event.altKey || event.metaKey)
      return;
    // CameraCapture is nested inside PromptInput. Its modal owns keyboard input
    // while open, even though the dialog remains a descendant of this form.
    if (form.ownerDocument.querySelector("dialog[open]")) return;
    event.preventDefault();
    event.stopPropagation();
    if (event.repeat) return;

    onSubmit();
  };
  form.addEventListener("keydown", onKeyDown, true);
  onCleanup(() => form.removeEventListener("keydown", onKeyDown, true));
}
