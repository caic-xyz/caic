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
        const lines = v.split("\n");
        for (let i = 0; i < lines.length; i++) {
          if (i > 0) editable.appendChild(document.createElement("br"));
          if (lines[i]) editable.appendChild(document.createTextNode(lines[i]));
        }
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

/** Block-level tags that browsers inject inside contentEditable divs. */
const blockTags = new Set(["DIV", "P", "BLOCKQUOTE", "LI", "PRE"]);

/** Get text from the editable div, converting browser markup back to plain text. */
function getText(el: HTMLElement): string {
  let text = "";
  for (const node of el.childNodes) {
    if (node.nodeType === Node.TEXT_NODE) {
      text += node.textContent;
    } else if (node.nodeType === Node.ELEMENT_NODE) {
      const elem = node as HTMLElement;
      // Skip non-editable children.
      if (elem.contentEditable === "false") continue;
      if (elem.tagName === "BR") {
        text += "\n";
      } else if (blockTags.has(elem.tagName)) {
        // Chrome/Firefox wrap lines in <div> or <p> — treat as newline-delimited blocks.
        if (text.length > 0 && !text.endsWith("\n")) text += "\n";
        text += elem.textContent;
      } else {
        text += elem.textContent;
      }
    }
  }
  return text;
}
