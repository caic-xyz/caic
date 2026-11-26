// Shared informational popup attached to an existing trigger or a standalone disclosure.

import { children, createSignal, createEffect, createMemo, createUniqueId, Show, onCleanup, type JSX } from "solid-js";
import { Portal } from "solid-js/web";

import { boundTooltipText } from "../tooltipText";
import styles from "./Tooltip.module.css";

interface TooltipTrigger {
  ref: (element: HTMLElement) => void;
  readonly "aria-describedby": string | undefined;
  onPointerOver: (event: PointerEvent) => void;
  onPointerDown: (event: PointerEvent) => void;
  onMouseEnter: () => void;
  onMouseLeave: () => void;
  onFocus: () => void;
  onBlur: () => void;
  onClick: () => void;
}

interface Props {
  /** A formatter receives a fixed wall-clock time for each opening. */
  text?: string | ((openedAt: number) => string);
  /** Attach to a control that owns its own activation and focus. */
  trigger?: (attributes: TooltipTrigger) => JSX.Element;
  children?: JSX.Element;
  class?: string;
  disabled?: boolean;
  /** Keep standalone disclosure activation from triggering a containing control. */
  stopActivationPropagation?: boolean;
}

let dismissActiveTooltip: (() => void) | undefined;

export default function Tooltip(props: Props) {
  const [openedAt, setOpenedAt] = createSignal<number | null>(null);
  const disabled = () => props.disabled || !props.text;
  const text = createMemo(() => {
    const at = openedAt();
    if (at === null || disabled()) return "";
    const value = props.text;
    return boundTooltipText(typeof value === "function" ? value(at) : (value ?? ""));
  });
  const popupId = createUniqueId();
  let triggerRef: HTMLElement | undefined;
  let popupRef: HTMLSpanElement | undefined;
  let touch = false;
  let hovering = false;
  let focused = false;

  function open() {
    if (disabled() || openedAt() !== null) return;
    dismissActiveTooltip?.();
    dismissActiveTooltip = dismiss;
    setOpenedAt(Date.now());
  }

  function dismiss() {
    if (dismissActiveTooltip === dismiss) dismissActiveTooltip = undefined;
    setOpenedAt(null);
  }

  function handleMouseEnter() {
    if (touch) return;
    hovering = true;
    open();
  }

  function handleMouseLeave() {
    if (touch) return;
    hovering = false;
    if (!focused) dismiss();
  }

  function toggle(event: MouseEvent | KeyboardEvent) {
    if (props.stopActivationPropagation) event.stopPropagation();
    if (openedAt() === null) open();
    else dismiss();
  }

  const trigger: TooltipTrigger = {
    ref: (el) => {
      if (triggerRef && triggerRef !== el) dismiss();
      triggerRef = el;
    },
    get "aria-describedby"() {
      return text() ? popupId : undefined;
    },
    onPointerOver: (event) => {
      touch = event.pointerType === "touch";
    },
    onPointerDown: (event) => {
      touch = event.pointerType === "touch";
      focused = false;
    },
    onMouseEnter: handleMouseEnter,
    onMouseLeave: handleMouseLeave,
    onFocus: () => {
      if (touch) return;
      focused = triggerRef?.matches(":focus-visible") ?? false;
      open();
    },
    onBlur: () => {
      focused = false;
      touch = false;
      if (!hovering) dismiss();
    },
    onClick: dismiss,
  };

  createEffect(() => {
    if (disabled()) dismiss();
  });

  createEffect(() => {
    if (!text()) return;
    const onOutsideClick = (event: MouseEvent) => {
      const target = event.target as Node;
      if (!triggerRef?.contains(target)) dismiss();
    };
    const onEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      const overlay = document.querySelector('dialog[open], [role="dialog"], [role="menu"], [role="listbox"]');
      if (overlay && !overlay.contains(triggerRef ?? null)) return;
      dismiss();
      event.preventDefault();
      event.stopImmediatePropagation();
    };

    // Only a scroll that can move the trigger detaches the popup from it; a
    // streaming transcript elsewhere on the page must not close it.
    const onScroll = (event: Event) => {
      const target = event.target;
      if (!(target instanceof Node) || !triggerRef || target.contains(triggerRef)) dismiss();
    };

    document.addEventListener("click", onOutsideClick, true);
    document.addEventListener("scroll", onScroll, true);
    document.addEventListener("keydown", onEscape, true);
    window.addEventListener("resize", dismiss);
    onCleanup(() => {
      document.removeEventListener("click", onOutsideClick, true);
      document.removeEventListener("scroll", onScroll, true);
      document.removeEventListener("keydown", onEscape, true);
      window.removeEventListener("resize", dismiss);
    });
  });

  createEffect(() => {
    if (!text() || !popupRef || !triggerRef) return;
    const anchor = triggerRef.getBoundingClientRect();
    const popup = popupRef.getBoundingClientRect();
    const gap = parseFloat(getComputedStyle(popupRef).getPropertyValue("--tooltip-gap")) || 0;
    const margin = 4;
    const above = anchor.top - gap - popup.height;
    const top = above >= margin ? above : anchor.bottom + gap;
    const clampedTop = Math.max(margin, Math.min(top, window.innerHeight - popup.height - margin));
    const left = anchor.left + (anchor.width - popup.width) / 2;

    popupRef.style.top = `${clampedTop}px`;
    popupRef.style.left = `${Math.max(margin, Math.min(left, window.innerWidth - popup.width - margin))}px`;
  });

  onCleanup(dismiss);

  // Resolve children once: each props.children read builds new DOM, so the
  // enabled and disabled wrappers would otherwise swap in fresh nodes and
  // detach any ref or observer a caller holds on the old ones.
  const content = children(() => props.children);
  const wrapperClass = () => `${styles.wrapper}${props.class ? ` ${props.class}` : ""}`;

  return (
    <>
      <Show
        when={props.trigger}
        fallback={
          <Show when={!disabled()} fallback={<span class={wrapperClass()}>{content()}</span>}>
            <span
              {...trigger}
              class={wrapperClass()}
              role="button"
              tabIndex={0}
              onClick={toggle}
              onKeyDown={(event) => {
                if (event.key !== "Enter" && event.key !== " ") return;
                event.preventDefault();
                toggle(event);
              }}
            >
              {content()}
            </span>
          </Show>
        }
      >
        {(renderTrigger) => renderTrigger()(trigger)}
      </Show>
      <Show when={text()}>
        <Portal mount={triggerRef?.closest("dialog[open]") ?? undefined}>
          <span ref={popupRef} id={popupId} role="tooltip" class={styles.popup}>
            <span class={styles.content}>{text()}</span>
          </span>
        </Portal>
      </Show>
    </>
  );
}
