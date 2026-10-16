// Shared task subview controls own pane closing, URL-backed full screen, focus, and Escape precedence.
import { Show, onCleanup, onMount, type JSX } from "solid-js";
import { useLocation, useNavigate, useSearchParams } from "@solidjs/router";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import FullscreenIcon from "@material-symbols/svg-400/outlined/fullscreen.svg?solid";
import FullscreenExitIcon from "@material-symbols/svg-400/outlined/fullscreen_exit.svg?solid";
import { taskViewMode } from "../taskView";
import ActionButton from "./ActionButton";
import styles from "./TaskViewHeader.module.css";

export default function TaskViewHeader(props: {
  taskPath: string;
  label: string;
  children: JSX.Element;
  actions?: JSX.Element;
  keyboardTarget?: () => HTMLElement | undefined;
}) {
  const location = useLocation();
  const navigate = useNavigate();
  const [, setQuery] = useSearchParams();
  const fullscreen = () => taskViewMode(location.pathname, location.search) === "fullscreen";
  let closeButton: HTMLButtonElement | undefined;
  let fullscreenButton: HTMLButtonElement | undefined;
  function close() {
    navigate(props.taskPath);
  }
  function toggleFullscreen() {
    setQuery({ fullscreen: fullscreen() ? undefined : "1" }, { replace: false });
  }

  onMount(() => {
    (fullscreen() ? fullscreenButton : closeButton)?.focus({ preventScroll: true });
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented || document.querySelector("dialog[open]")) return;
      const target = event.target instanceof Element ? event.target : null;
      if (target?.closest('[role="dialog"], [role="menu"]')) return;
      event.preventDefault();
      event.stopPropagation();
      if (fullscreen()) {
        toggleFullscreen();
        fullscreenButton?.focus({ preventScroll: true });
      } else close();
    };
    // Embedded desktops consume canvas keydowns. Capture full-screen dismissal
    // on their host after document-level menus and overlays have handled Escape.
    const keyboardTarget = props.keyboardTarget?.();
    const onEmbeddedKey = (event: KeyboardEvent) => {
      if (fullscreen()) onKey(event);
    };
    keyboardTarget?.addEventListener("keydown", onEmbeddedKey, true);
    document.addEventListener("keydown", onKey);
    onCleanup(() => {
      keyboardTarget?.removeEventListener("keydown", onEmbeddedKey, true);
      document.removeEventListener("keydown", onKey);
    });
  });

  return (
    <header class={styles.header} aria-label={props.label}>
      {props.children}
      {props.actions}
      <div class={styles.paneControls}>
        <ActionButton
          ref={fullscreenButton}
          onClick={toggleFullscreen}
          label={fullscreen() ? "Exit full screen" : "Full screen"}
        >
          <Show when={fullscreen()} fallback={<FullscreenIcon width={20} height={20} aria-hidden="true" />}>
            <FullscreenExitIcon width={20} height={20} aria-hidden="true" />
          </Show>
        </ActionButton>
        <ActionButton ref={closeButton} onClick={close} label="Close view">
          <CloseIcon width={20} height={20} aria-hidden="true" />
        </ActionButton>
      </div>
    </header>
  );
}
