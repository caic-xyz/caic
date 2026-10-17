// Corner CAD controls reveal camera reset and axis visibility on hover, tap, or keyboard focus.
import { Show, createEffect, createSignal, createUniqueId, onCleanup, type JSX } from "solid-js";
import ResetIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";
import ViewIcon from "@material-symbols/svg-400/outlined/3d_rotation.svg?solid";
import ShowIcon from "@material-symbols/svg-400/outlined/visibility.svg?solid";
import HideIcon from "@material-symbols/svg-400/outlined/visibility_off.svg?solid";
import ActionButton from "./ActionButton";
import styles from "./CadViewControls.module.css";

export default function CadViewControls(props: {
  axesVisible: boolean;
  onToggleAxes: () => void;
  onResetView: () => void;
  children: JSX.Element;
}) {
  const [open, setOpen] = createSignal(false);
  const panelId = createUniqueId();
  let viewport: HTMLDivElement | undefined;
  let corner: HTMLDivElement | undefined;
  let panel: HTMLDivElement | undefined;
  let trigger: HTMLButtonElement | undefined;
  let hovered = false;
  let pinned = false;
  let triggerWasOpen = false;
  let triggerPointerType: string | null = null;
  let start: { x: number; y: number; moved: boolean } | null = null;

  function containsPoint(rect: DOMRect, x: number, y: number) {
    return x >= rect.left && x <= rect.right && y >= rect.top && y <= rect.bottom;
  }

  function overCorner(x: number, y: number) {
    return corner ? containsPoint(corner.getBoundingClientRect(), x, y) : false;
  }

  function dismiss(restoreFocus: boolean) {
    if (restoreFocus) trigger?.focus({ preventScroll: true });
    pinned = false;
    hovered = false;
    setOpen(false);
  }

  function closeIfUnoccupied() {
    if (!hovered && !pinned && !corner?.querySelector(":focus-visible")) setOpen(false);
  }

  function togglePanel(pointerType: string | null, wasOpen: boolean) {
    // Mouse clicks pin a hover disclosure; touch and keyboard toggle expansion.
    if (pointerType === "mouse") {
      pinned = !pinned;
      setOpen(pinned || hovered);
    } else {
      pinned = !wasOpen;
      setOpen(pinned);
    }
  }

  function pointerMove(event: PointerEvent) {
    if (start && Math.hypot(event.clientX - start.x, event.clientY - start.y) > 6) {
      start.moved = true;
      dismiss(false);
    }
    if (event.pointerType !== "mouse" || event.buttons !== 0) return;
    const bounds = corner?.getBoundingClientRect();
    const menuBounds = open() ? panel?.getBoundingClientRect() : null;
    // Include the gap so moving from the axes to their controls keeps them open.
    hovered = Boolean(
      bounds &&
      event.clientX >= Math.min(bounds.left, menuBounds?.left ?? bounds.left) &&
      event.clientX <= bounds.right &&
      event.clientY >= bounds.top &&
      event.clientY <= (menuBounds?.bottom ?? bounds.bottom),
    );
    if (hovered) setOpen(true);
    else closeIfUnoccupied();
  }

  function pointerDown(event: PointerEvent) {
    if (!(event.target instanceof HTMLCanvasElement)) return;
    if (event.button !== 0 || !event.isPrimary) {
      start = null;
      return;
    }
    start = { x: event.clientX, y: event.clientY, moved: false };
  }

  function pointerUp(event: PointerEvent) {
    if (start && !start.moved && overCorner(start.x, start.y) && overCorner(event.clientX, event.clientY)) {
      togglePanel(event.pointerType, open());
    }
    start = null;
  }

  createEffect(() => {
    if (!open()) return;
    const outside = (event: PointerEvent) => {
      if (
        corner?.contains(event.target as Node) ||
        (viewport?.contains(event.target as Node) && overCorner(event.clientX, event.clientY))
      )
        return;
      dismiss(false);
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented || document.querySelector("dialog[open]")) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      dismiss(Boolean(corner?.contains(document.activeElement)));
    };
    document.addEventListener("pointerdown", outside, true);
    document.addEventListener("keydown", escape, true);
    onCleanup(() => {
      document.removeEventListener("pointerdown", outside, true);
      document.removeEventListener("keydown", escape, true);
    });
  });

  function activate(action: () => void) {
    action();
    dismiss(true);
  }

  return (
    <div
      ref={viewport}
      class={styles.viewport}
      onPointerMove={pointerMove}
      onPointerDown={pointerDown}
      onPointerUp={pointerUp}
      onPointerCancel={() => (start = null)}
      onPointerLeave={() => {
        hovered = false;
        closeIfUnoccupied();
      }}
    >
      {props.children}
      <div
        ref={corner}
        class={styles.corner}
        classList={{ [styles.compact]: !props.axesVisible }}
        onFocusIn={() => setOpen(true)}
        onFocusOut={(event) => {
          if (!hovered && !pinned && !corner?.contains(event.relatedTarget as Node | null)) setOpen(false);
        }}
      >
        <ActionButton
          ref={trigger}
          class={`${styles.trigger}${props.axesVisible ? ` ${styles.transparent}` : ""}`}
          label="View controls"
          aria-expanded={open()}
          aria-controls={panelId}
          onPointerDown={(event) => {
            triggerWasOpen = open();
            triggerPointerType = event.pointerType;
          }}
          onClick={(event) => {
            // Pointer focus reveals the panel before click; keyboard focus may
            // already have revealed it, so each activation toggles its state.
            togglePanel(event.detail === 0 ? null : triggerPointerType, event.detail === 0 ? open() : triggerWasOpen);
          }}
        >
          <Show when={!props.axesVisible}>
            <ViewIcon width={20} height={20} aria-hidden="true" />
          </Show>
        </ActionButton>
        <Show when={open()}>
          <div ref={panel} id={panelId} class={styles.panel} role="group" aria-label="View controls">
            <ActionButton label="Reset view" onClick={() => activate(props.onResetView)}>
              <ResetIcon width={20} height={20} aria-hidden="true" />
              Reset view
            </ActionButton>
            <ActionButton
              label={props.axesVisible ? "Hide axes" : "Show axes"}
              onClick={() => activate(props.onToggleAxes)}
            >
              <Show when={props.axesVisible} fallback={<ShowIcon width={20} height={20} aria-hidden="true" />}>
                <HideIcon width={20} height={20} aria-hidden="true" />
              </Show>
              {props.axesVisible ? "Hide axes" : "Show axes"}
            </ActionButton>
          </div>
        </Show>
      </div>
    </div>
  );
}
