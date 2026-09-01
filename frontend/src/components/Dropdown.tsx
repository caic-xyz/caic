// Reusable dropdown menu with arrow navigation, dismissal, and trigger focus restoration.

import { createEffect, onCleanup, onMount, Show, type JSX } from "solid-js";

interface DropdownProps {
  /** Whether the dropdown is open. */
  open: boolean;
  /** Called when the dropdown should close (click outside) or open state changes. */
  onOpenChange: (open: boolean) => void;
  /** CSS class for the wrapper element.
   *
   * The wrapper is the positioning anchor — it should have `position: relative`
   * so that the dropdown content (which the caller styles with `position: absolute`)
   * is positioned relative to it. */
  class?: string;
  /** Trigger / children rendered inside the wrapper (e.g. the toggle button). */
  children: JSX.Element;
  /** Dropdown content, shown when `open` is true. */
  content: JSX.Element;
}

/** Dropdown menu that dismisses on click outside.
 *
 * Wraps the trigger (children) and dropdown content in a single container,
 * managing click-outside dismissal via a capture-phase document listener. */
export default function Dropdown(props: DropdownProps) {
  // eslint-disable-next-line no-unassigned-vars -- assigned by SolidJS ref
  let containerRef: HTMLDivElement | undefined;

  const menuItems = (menu: Element) => Array.from(
    menu.querySelectorAll<HTMLElement>("[role='menuitem']:not([disabled]):not([aria-disabled='true'])"),
  );

  function handleKeyDown(event: KeyboardEvent) {
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
    event.preventDefault();
    event.stopPropagation();

    const menu = (event.target as Element).closest("[role='menu']");
    if (!menu) {
      props.onOpenChange(true);
      requestAnimationFrame(() => {
        const openMenu = containerRef?.querySelector("[role='menu']");
        if (!openMenu) return;
        const openItems = menuItems(openMenu);
        openItems[event.key === "ArrowUp" ? openItems.length - 1 : 0]?.focus();
      });
      return;
    }

    const items = menuItems(menu);
    const current = items.indexOf(document.activeElement as HTMLElement);
    const delta = event.key === "ArrowDown" ? 1 : -1;
    const next = current < 0
      ? (delta > 0 ? 0 : items.length - 1)
      : (current + delta + items.length) % items.length;
    items[next]?.focus();
  }

  onMount(() => {
    containerRef?.addEventListener("keydown", handleKeyDown);
    onCleanup(() => containerRef?.removeEventListener("keydown", handleKeyDown));
  });

  createEffect(() => {
    if (!props.open) return;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const onClickOutside = (event: MouseEvent) => {
      if (containerRef && !containerRef.contains(event.target as Node)) props.onOpenChange(false);
    };
    const onEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopImmediatePropagation();
      props.onOpenChange(false);
      opener?.focus();
    };
    document.addEventListener("click", onClickOutside, true);
    document.addEventListener("keydown", onEscape, true);
    onCleanup(() => {
      document.removeEventListener("click", onClickOutside, true);
      document.removeEventListener("keydown", onEscape, true);
    });
  });

  return (
    <div ref={containerRef} class={props.class}>
      {props.children}
      <Show when={props.open}>{props.content}</Show>
    </div>
  );
}
