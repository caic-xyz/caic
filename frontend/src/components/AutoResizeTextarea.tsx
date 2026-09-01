// Auto-resizing contenteditable prompt with keyboard submission and caret restoration.
// Uses contenteditable with an optional CSS ::before float spacer so text wraps
// around absolutely positioned trailing buttons.
// Enter submits (via onSubmit), Shift+Enter inserts a newline.

import { createEffect, onCleanup, onMount } from "solid-js";

import styles from "./AutoResizeTextarea.module.css";

const emptyClass = styles.empty;

interface Props {
  value: string;
  onInput: (value: string) => void;
  onSubmit?: () => void;
  onKeyDown?: (event: KeyboardEvent) => void;
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
  let focusGeneration = 0;
  let pointerFocus = false;
  let savedRange: Range | undefined;

  function rememberCaret() {
    const selection = window.getSelection();
    if (!selection || selection.rangeCount === 0) return;
    const range = selection.getRangeAt(0);
    if (editable.contains(range.commonAncestorContainer)) savedRange = range.cloneRange();
  }

  function restoreCaret() {
    if (pointerFocus) return;
    const generation = ++focusGeneration;
    const previousRange = savedRange && editable.contains(savedRange.commonAncestorContainer)
      ? savedRange.cloneRange()
      : undefined;
    queueMicrotask(() => {
      if (generation !== focusGeneration || document.activeElement !== editable) return;
      const selection = window.getSelection();
      if (!selection) return;
      const range = previousRange ?? document.createRange();
      if (!previousRange) {
        range.selectNodeContents(editable);
        range.collapse(false);
      }
      selection.removeAllRanges();
      selection.addRange(range);
    });
  }

  function handlePointerDown() {
    pointerFocus = true;
    queueMicrotask(() => { pointerFocus = false; });
  }

  function handleBlur() {
    focusGeneration++;
    rememberCaret();
  }

  onMount(() => {
    document.addEventListener("selectionchange", rememberCaret);
    onCleanup(() => document.removeEventListener("selectionchange", rememberCaret));
  });

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
    props.onKeyDown?.(e);
    if (e.defaultPrevented) return;
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
      contentEditable={!props.disabled}
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
      onPointerDown={handlePointerDown}
      onFocus={restoreCaret}
      onBlur={handleBlur}
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
