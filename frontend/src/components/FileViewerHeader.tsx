// Shared document controls own close, URL-backed maximize/restore, raw access, and Escape handling.
import { Show, onCleanup, onMount, type JSX } from "solid-js";
import { useLocation, useNavigate, useSearchParams } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import FullscreenIcon from "@material-symbols/svg-400/outlined/fullscreen.svg?solid";
import FullscreenExitIcon from "@material-symbols/svg-400/outlined/fullscreen_exit.svg?solid";
import { fileViewMode } from "../fileView";
import { taskRawURL } from "../runtimePath";
import ActionButton from "./ActionButton";
import styles from "./FileViewerHeader.module.css";

export default function FileViewerHeader(props: {
  taskId: string;
  taskPath: string;
  path: string;
  children?: JSX.Element;
}) {
  const location = useLocation();
  const navigate = useNavigate();
  const [, setQuery] = useSearchParams();
  const fullscreen = () => fileViewMode(location.pathname, location.search) === "fullscreen";
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
      if (fullscreen()) toggleFullscreen();
      else close();
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  return (
    <header class={styles.header} aria-label="Document controls">
      <Show when={!fullscreen()}>
        <ActionButton ref={closeButton} onClick={close} label="Close document">
          <ArrowBackIcon width={20} height={20} />
        </ActionButton>
      </Show>
      <span class={styles.path} title={props.path}>
        {props.path}
      </span>
      <ActionButton
        ref={fullscreenButton}
        onClick={toggleFullscreen}
        label={fullscreen() ? "Exit full screen" : "Full screen"}
      >
        <Show when={fullscreen()} fallback={<FullscreenIcon width={20} height={20} aria-hidden="true" />}>
          <FullscreenExitIcon width={20} height={20} aria-hidden="true" />
        </Show>
      </ActionButton>
      <a class={styles.rawLink} href={taskRawURL(props.taskId, props.path)} target="_blank" rel="noopener noreferrer">
        raw file
      </a>
      {props.children}
    </header>
  );
}
