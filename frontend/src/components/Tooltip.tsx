// Tooltip opens on hover, focus, or tap and provides formatters with its opening time.
// Uses a Portal so the popup is never clipped by overflow: hidden ancestors.

import { createSignal, createEffect, createMemo, createUniqueId, Show, onCleanup, type JSX } from "solid-js";
import { Portal } from "solid-js/web";

import styles from "./Tooltip.module.css";

interface Props {
  /** A formatter receives a fixed wall-clock time for each opening. */
  text?: string | ((openedAt: number) => string);
  children: JSX.Element;
  class?: string;
  disabled?: boolean;
  /** Keep activation from triggering a containing control. */
  stopActivationPropagation?: boolean;
}

const GAP = 6; // px between element and popup

export default function Tooltip(props: Props) {
  const [openedAt, setOpenedAt] = createSignal<number | null>(null);
  const show = () => openedAt() !== null;
  const text = createMemo(() => {
    const at = openedAt();
    if (at === null) return "";
    const value = props.text;
    return typeof value === "function" ? value(at) : (value ?? "");
  });
  const popupId = createUniqueId();
  let wrapperRef: HTMLSpanElement | undefined;
  let popupRef: HTMLSpanElement | undefined;
  let touch = false;

  function onDocClick(e: MouseEvent) {
    if (wrapperRef && !wrapperRef.contains(e.target as Node)) {
      dismiss();
    }
  }

  function open() {
    setOpenedAt((at) => at ?? Date.now());
  }

  function dismiss() {
    setOpenedAt(null);
  }

  function toggle() {
    if (show()) dismiss();
    else open();
  }

  createEffect(() => {
    if (show()) {
      document.addEventListener("click", onDocClick, true);
      document.addEventListener("scroll", dismiss, true);
    } else {
      document.removeEventListener("click", onDocClick, true);
      document.removeEventListener("scroll", dismiss, true);
    }
  });

  // Position the popup in fixed coordinates relative to the wrapper.
  createEffect(() => {
    if (!show() || !text() || !popupRef || !wrapperRef) return;
    const wr = wrapperRef.getBoundingClientRect();
    const pr = popupRef.getBoundingClientRect();
    const margin = 4;

    // Prefer above, then below, and keep tall content in the viewport.
    const above = wr.top - GAP - pr.height;
    const top = above >= margin ? above : wr.bottom + GAP;
    popupRef.style.top = `${Math.max(margin, Math.min(top, window.innerHeight - pr.height - margin))}px`;

    // Horizontal: center on wrapper, clamp to viewport.
    let left = wr.left + wr.width / 2 - pr.width / 2;
    left = Math.max(margin, Math.min(left, window.innerWidth - pr.width - margin));
    popupRef.style.left = `${left}px`;
  });

  onCleanup(() => {
    document.removeEventListener("click", onDocClick, true);
    document.removeEventListener("scroll", dismiss, true);
  });

  const wrapperClass = () => `${styles.wrapper}${props.class ? ` ${props.class}` : ""}`;

  const disabled = () => props.disabled || !props.text;

  return (
    <Show when={!disabled()} fallback={<span class={wrapperClass()}>{props.children}</span>}>
      <span
        ref={wrapperRef}
        class={wrapperClass()}
        role="button"
        tabIndex={0}
        aria-describedby={show() ? popupId : undefined}
        // Touch generates compatibility mouse and focus events before click.
        // Only click toggles visibility for that sequence.
        onPointerOver={(e) => {
          touch = e.pointerType === "touch";
        }}
        onPointerDown={(e) => {
          touch = e.pointerType === "touch";
        }}
        onMouseEnter={() => {
          if (!touch) open();
        }}
        onMouseLeave={() => {
          if (!touch) dismiss();
        }}
        onFocus={() => {
          if (!touch) open();
        }}
        onBlur={() => {
          touch = false;
          dismiss();
        }}
        onClick={(e) => {
          if (props.stopActivationPropagation) e.stopPropagation();
          toggle();
        }}
        onKeyDown={(e) => {
          if (e.key === "Escape" && show()) {
            e.preventDefault();
            e.stopPropagation();
            dismiss();
          }
          if (e.key === "Enter" || e.key === " ") {
            if (props.stopActivationPropagation) e.stopPropagation();
            e.preventDefault();
            toggle();
          }
        }}
      >
        {props.children}
        <Show when={show()}>
          <Portal>
            <span ref={popupRef} id={popupId} role="tooltip" class={styles.popup}>
              {text()}
            </span>
          </Portal>
        </Show>
      </span>
    </Show>
  );
}
