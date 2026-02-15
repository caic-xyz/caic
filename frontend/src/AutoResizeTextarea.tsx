// Auto-resizing textarea that starts as a single line and expands vertically.
// Uses CSS field-sizing: content for native auto-resize.
// Enter submits (via onSubmit), Shift+Enter inserts a newline.
import type { JSX } from "solid-js";

interface Props {
  value: string;
  onInput: (value: string) => void;
  onSubmit?: () => void;
  placeholder?: string;
  disabled?: boolean;
  class?: string;
  ref?: (el: HTMLTextAreaElement) => void;
  tabIndex?: number;
}

export default function AutoResizeTextarea(props: Props) {
  const handleInput: JSX.EventHandler<HTMLTextAreaElement, InputEvent> = (e) => {
    props.onInput(e.currentTarget.value);
  };

  const handleKeyDown: JSX.EventHandler<HTMLTextAreaElement, KeyboardEvent> = (e) => {
    if (e.key === "Enter" && !e.shiftKey && props.onSubmit) {
      e.preventDefault();
      props.onSubmit();
    }
  };

  return (
    <textarea
      ref={props.ref}
      value={props.value}
      onInput={handleInput}
      onKeyDown={handleKeyDown}
      placeholder={props.placeholder}
      disabled={props.disabled}
      class={props.class}
      tabIndex={props.tabIndex}
      rows={1}
      style={{ "field-sizing": "content" }}
    />
  );
}
