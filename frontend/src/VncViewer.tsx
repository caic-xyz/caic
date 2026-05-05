// noVNC-based desktop viewer for a task's container display.
import { createSignal, onMount, onCleanup, Show } from "solid-js";
import { useNavigate } from "@solidjs/router";
import RFB from "@novnc/novnc";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import FullscreenIcon from "@material-symbols/svg-400/outlined/fullscreen.svg?solid";
import FullscreenExitIcon from "@material-symbols/svg-400/outlined/fullscreen_exit.svg?solid";
import styles from "./VncViewer.module.css";

interface Props {
  taskId: string;
  repo: string;
  branch: string;
  taskPath: string;
}

export default function VncViewer(props: Props) {
  const navigate = useNavigate();
  const [error, setError] = createSignal<string | null>(null);
  const [fullscreen, setFullscreen] = createSignal(false);
  let containerRef: HTMLDivElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref
  let canvasRef: HTMLDivElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref

  onMount(() => {
    const canvas = canvasRef;
    if (!canvas) return;

    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${protocol}//${window.location.host}/api/v1/tasks/${props.taskId}/vnc/ws`;

    let rfb: InstanceType<typeof RFB>;
    try {
      rfb = new RFB(canvas, url);
      rfb.resizeSession = true;
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to connect");
      return;
    }

    onCleanup(() => {
      try { rfb.disconnect(); } catch { /* ignore */ }
    });
  });

  // Sync fullscreen state with browser Fullscreen API events.
  onMount(() => {
    const onFSChange = () => setFullscreen(!!document.fullscreenElement);
    document.addEventListener("fullscreenchange", onFSChange);
    onCleanup(() => document.removeEventListener("fullscreenchange", onFSChange));
  });

  function toggleFullscreen() {
    if (document.fullscreenElement) {
      document.exitFullscreen();
    } else {
      containerRef?.requestFullscreen();
    }
  }

  return (
    <div ref={containerRef} class={styles.container} classList={{ [styles.fullscreen]: fullscreen() }}>
      <div class={styles.header}>
        <Show when={!fullscreen()}>
          <button class={styles.backBtn} onClick={() => navigate(props.taskPath)} title="Back to task">
            <ArrowBackIcon width={20} height={20} />
          </button>
          <span class={styles.headerMeta}>
            <span class={styles.headerRepo}>{props.repo}</span>
            <span class={styles.headerBranch}>{props.branch}</span>
          </span>
        </Show>
        <button class={styles.backBtn} onClick={toggleFullscreen} title={fullscreen() ? "Exit fullscreen" : "Fullscreen"}>
          <Show when={fullscreen()} fallback={<FullscreenIcon width={20} height={20} />}>
            <FullscreenExitIcon width={20} height={20} />
          </Show>
        </button>
      </div>
      <div class={styles.content}>
        <Show when={error()}>
          <div class={styles.error}>{error()}</div>
        </Show>
        <div ref={canvasRef} class={styles.canvas} />
      </div>
    </div>
  );
}
