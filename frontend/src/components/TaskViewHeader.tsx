// Shared task subview controls own history Back, URL-backed full screen, focus, and Escape precedence.
import { Show, onCleanup, onMount, type JSX } from "solid-js";
import { useLocation, useNavigate, useSearchParams } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
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
  let backButton: HTMLButtonElement | undefined;
  let fullscreenButton: HTMLButtonElement | undefined;
  function toggleFullscreen() {
    setQuery({ fullscreen: fullscreen() ? undefined : "1" }, { replace: false });
  }

  onMount(() => {
    (fullscreen() ? fullscreenButton : backButton)?.focus({ preventScroll: true });
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented || document.querySelector("dialog[open]")) return;
      const target = event.target instanceof Element ? event.target : null;
      if (target?.closest('[role="dialog"], [role="menu"]')) return;
      event.preventDefault();
      event.stopPropagation();
      if (fullscreen()) {
        toggleFullscreen();
        fullscreenButton?.focus({ preventScroll: true });
      } else navigate(props.taskPath);
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
      <Show when={!fullscreen()}>
        <ActionButton ref={backButton} onClick={() => navigate(-1)} label="Back">
          <ArrowBackIcon width={20} height={20} aria-hidden="true" />
        </ActionButton>
      </Show>
      {props.children}
      <ActionButton
        ref={fullscreenButton}
        onClick={toggleFullscreen}
        label={fullscreen() ? "Exit full screen" : "Full screen"}
      >
        <Show when={fullscreen()} fallback={<FullscreenIcon width={20} height={20} aria-hidden="true" />}>
          <FullscreenExitIcon width={20} height={20} aria-hidden="true" />
        </Show>
      </ActionButton>
      {props.actions}
    </header>
  );
}
