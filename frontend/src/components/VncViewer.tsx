// noVNC-based container display within the URL-backed task workspace.

import { createSignal, onMount, onCleanup, Show } from "solid-js";
import RFB from "@novnc/novnc";
import TaskViewHeader from "./TaskViewHeader";

import styles from "./VncViewer.module.css";

interface Props {
  taskId: string;
  taskPath: string;
  repo: string;
  branch: string;
}

export default function VncViewer(props: Props) {
  const [error, setError] = createSignal<string | null>(null);
  let canvasRef: HTMLDivElement | undefined;

  onMount(() => {
    const canvas = canvasRef;
    if (!canvas) return;

    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${protocol}//${window.location.host}/api/caic/v1/tasks/${props.taskId}/vnc/ws`;

    let rfb: InstanceType<typeof RFB>;
    try {
      rfb = new RFB(canvas, url, { shared: false });
      rfb.resizeSession = true;
      rfb.scaleViewport = true;
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to connect");
      return;
    }

    function onDisconnect(e: Event) {
      // noVNC 1.7.0 disconnect detail only has { clean: boolean }.
      const de = e as CustomEvent<{ clean: boolean }>;
      if (!de.detail.clean) {
        setError("VNC connection failed — check that the container has a display enabled");
      }
    }

    function onConnect(_e: Event) {
      setError(null);
    }

    rfb.addEventListener("disconnect", onDisconnect);
    rfb.addEventListener("connect", onConnect);

    onCleanup(() => {
      try {
        rfb.removeEventListener("disconnect", onDisconnect);
        rfb.removeEventListener("connect", onConnect);
        rfb.disconnect();
      } catch {
        /* ignore */
      }
    });
  });

  return (
    <div class={styles.container}>
      <TaskViewHeader taskPath={props.taskPath} label="VNC controls" keyboardTarget={() => canvasRef}>
        <span class={styles.headerMeta}>
          <span class={styles.headerRepo}>{props.repo}</span>
          <span class={styles.headerBranch}>{props.branch}</span>
        </span>
      </TaskViewHeader>
      <div class={styles.content}>
        <Show when={error()}>
          <div class={styles.error}>{error()}</div>
        </Show>
        <div ref={canvasRef} class={styles.canvas} />
      </div>
    </div>
  );
}
