// Task subview header manages immersive documents, return navigation, focus, and Escape.
import { onCleanup, onMount, type JSX } from "solid-js";
import { useLocation, useNavigate } from "@solidjs/router";
import { isTaskViewEscape, taskViewMode, type TaskViewNavigationState } from "../taskView";
import TaskViewControls, { useTaskFullscreenToggle } from "./TaskViewControls";
import styles from "./TaskViewHeader.module.css";

export default function TaskViewHeader(props: {
  taskPath: string;
  label: string;
  children: JSX.Element;
  actions?: JSX.Element;
  keyboardTarget?: () => HTMLElement | undefined;
}) {
  const location = useLocation<TaskViewNavigationState>();
  const navigate = useNavigate();
  const fullscreen = () => taskViewMode(location.pathname, location.search) === "fullscreen";
  const toggleFullscreen = useTaskFullscreenToggle(fullscreen);
  const immersive = () => fullscreen() && location.pathname.endsWith("/view");
  let closeButton: HTMLButtonElement | undefined;
  let fullscreenButton: HTMLButtonElement | undefined;
  function close() {
    navigate(location.state?.returnToFullscreen === true ? `${props.taskPath}?fullscreen=1` : props.taskPath);
  }

  onMount(() => {
    // Mobile hides the close button in favor of the back button, so focus
    // falls through to full screen when the close button cannot take it.
    if (!fullscreen()) closeButton?.focus({ preventScroll: true });
    if (document.activeElement !== closeButton) fullscreenButton?.focus({ preventScroll: true });
    const onKey = (event: KeyboardEvent) => {
      if (!isTaskViewEscape(event)) return;
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
    <header class={styles.header} hidden={immersive()} aria-label={props.label}>
      {props.children}
      {props.actions}
      <TaskViewControls
        fullscreen={fullscreen()}
        toggleFullscreen={toggleFullscreen}
        fullscreenRef={(element) => {
          fullscreenButton = element;
        }}
        close={close}
        closeLabel="Close view"
        closeRef={(element) => {
          closeButton = element;
        }}
      />
    </header>
  );
}
