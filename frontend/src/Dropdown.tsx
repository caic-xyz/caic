// Reusable dropdown menu with click-outside dismissal.
import { createEffect, onCleanup, Show, type JSX } from "solid-js";

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

  createEffect(() => {
    const onClickOutside = (e: MouseEvent) => {
      if (containerRef && !containerRef.contains(e.target as Node)) {
        props.onOpenChange(false);
      }
    };
    if (props.open) {
      document.addEventListener("click", onClickOutside, true);
    } else {
      document.removeEventListener("click", onClickOutside, true);
    }
    onCleanup(() => document.removeEventListener("click", onClickOutside, true));
  });

  return (
    <div ref={containerRef} class={props.class}>
      {props.children}
      <Show when={props.open}>{props.content}</Show>
    </div>
  );
}
