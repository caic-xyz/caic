// Auto-resizing editable div that starts as a single line and expands vertically.
// Uses contenteditable with an optional CSS ::before float spacer so text wraps
// around absolutely positioned trailing buttons.
// Enter submits (via onSubmit), Shift+Enter inserts a newline.
import { createEffect } from "solid-js";
import styles from "./AutoResizeTextarea.module.css";

const emptyClass = styles.empty;

interface Props {
  value: string;
  onInput: (value: string) => void;
  onSubmit?: () => void;
  placeholder?: string;
  disabled?: boolean;
  class?: string;
  ref?: (el: HTMLDivElement) => void;
  tabIndex?: number;
  "data-testid"?: string;
  /** CSS class added to the editable that activates a ::before float spacer
   *  so text wraps around trailing buttons. */
  spacerClass?: string;
}

export default function AutoResizeTextarea(props: Props) {
  let editable!: HTMLDivElement;

  // Sync external value changes (e.g. cleared after submit) into the DOM
  // without disrupting in-progress typing.
  createEffect(() => {
    const v = props.value;
    if (getText(editable) !== v) {
      editable.textContent = "";
      if (v) {
        editable.appendChild(document.createTextNode(v));
      }
      editable.classList.toggle(emptyClass, v.length === 0);
    }
  });

  function handleInput() {
    const text = getText(editable);
    editable.classList.toggle(emptyClass, text.length === 0);
    props.onInput(text);
  }

  function handleKeyDown(e: KeyboardEvent) {
    if (e.key === "Enter" && !e.shiftKey && props.onSubmit) {
      e.preventDefault();
      props.onSubmit();
    }
  }

  // Paste as plain text only.
  function handlePaste(e: ClipboardEvent) {
    if (typeof e.clipboardData?.getData !== "function") return;
    const text = e.clipboardData.getData("text/plain");
    if (text !== undefined) {
      e.preventDefault();
      document.execCommand("insertText", false, text);
    }
  }

  return (
    <div
      ref={(el) => {
        editable = el;
        el.addEventListener("input", handleInput);
        props.ref?.(el);
      }}
      contentEditable={props.disabled ? "false" : "true"}
      role="textbox"
      aria-multiline="true"
      aria-label={props.placeholder}
      aria-placeholder={props.placeholder}
      aria-disabled={props.disabled || undefined}
      data-placeholder={props.placeholder}
      class={`${styles.editable}${props.value ? "" : ` ${emptyClass}`}${props.spacerClass ? ` ${props.spacerClass}` : ""}${props.class ? ` ${props.class}` : ""}`}
      tabIndex={props.tabIndex ?? 0}
      data-testid={props["data-testid"]}
      onKeyDown={handleKeyDown}
      onPaste={handlePaste}
    />

  );
}

/** Get text from the editable div, ignoring child elements. */
function getText(el: HTMLElement): string {
  let text = "";
  for (const node of el.childNodes) {
    if (node.nodeType === Node.TEXT_NODE) {
      text += node.textContent;
    } else if (node.nodeType === Node.ELEMENT_NODE) {
      const elem = node as HTMLElement;
      // Skip non-editable children.
      if (elem.contentEditable === "false") continue;
      // Handle <br> as newline.
      if (elem.tagName === "BR") {
        text += "\n";
      } else {
        text += elem.textContent;
      }
    }
  }
  return text;
}
