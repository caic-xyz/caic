// Full-page interactive 3D viewer that renders STL and STEP CAD files from a task runtime.

import { Show, createEffect, createSignal, onCleanup, onMount } from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";

import styles from "./CadViewer.module.css";
import { taskFileURL } from "../runtimePath";
import { readTaskBinaryFile } from "../taskFile";
import type { CadModelInfo, CadScene } from "../cad";

interface Props {
  taskId: string;
  taskPath: string;
  path: string;
}

export default function CadViewer(props: Props) {
  const navigate = useNavigate();
  let canvasRef: HTMLCanvasElement | undefined;
  let wrapRef: HTMLDivElement | undefined;
  let scene: CadScene | null = null;
  let resizeObserver: ResizeObserver | null = null;
  let abortController: AbortController | null = null;

  const [loading, setLoading] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  const [modelInfo, setModelInfo] = createSignal<CadModelInfo | null>(null);
  const [wireframe, setWireframe] = createSignal(false);
  const [webglAvailable, setWebglAvailable] = createSignal(true);

  // Escape navigates back to the task detail.
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        navigate(props.taskPath);
      }
    };
    document.addEventListener("keydown", onKey, true);
    onCleanup(() => document.removeEventListener("keydown", onKey, true));
  });

  function toggleWireframe() {
    const next = !wireframe();
    setWireframe(next);
    scene?.setWireframe(next);
  }

  function resetCamera() {
    scene?.resetCamera();
  }

  async function loadModelData(filePath: string) {
    abortController?.abort();
    abortController = new AbortController();
    setLoading(true);
    setError(null);
    setModelInfo(null);

    try {
      const buffer = await readTaskBinaryFile(props.taskId, filePath, abortController.signal);
      const { createCadScene, isWebGLSupported, parseStl, parseStep, createStlGroup, computeModelInfo } =
        await import("../cad");

      const hasWebGL = isWebGLSupported();
      setWebglAvailable(hasWebGL);

      if (hasWebGL && canvasRef) {
        if (!scene) {
          scene = createCadScene(canvasRef);
        }
        const info = await scene.loadModel(buffer, filePath);
        setModelInfo(info);
        if (wrapRef) {
          const rect = wrapRef.getBoundingClientRect();
          scene.resize(rect.width, rect.height);
        }
      } else {
        // Compute model info even without WebGL for accessibility and metadata.
        const isStl = /\.stl$/i.test(filePath);
        const group = isStl ? createStlGroup(parseStl(buffer)) : await parseStep(buffer);
        setModelInfo(computeModelInfo(group));
      }
      setLoading(false);
    } catch (err: unknown) {
      if (abortController?.signal.aborted) return;
      setLoading(false);
      const msg = err instanceof Error ? err.message : "Failed to load 3D model";
      setError(msg);
    }
  }

  onMount(() => {
    if (wrapRef) {
      resizeObserver = new ResizeObserver((entries) => {
        for (const entry of entries) {
          const { width, height } = entry.contentRect;
          scene?.resize(width, height);
        }
      });
      resizeObserver.observe(wrapRef);
    }

    onCleanup(() => {
      abortController?.abort();
      resizeObserver?.disconnect();
      scene?.destroy();
      scene = null;
    });
  });

  createEffect(() => {
    const p = props.path;
    void loadModelData(p);
  });

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button
          class={styles.backBtn}
          onClick={() => navigate(props.taskPath)}
          title="Back to task"
          aria-label="Back to task"
        >
          <ArrowBackIcon width={20} height={20} />
        </button>
        <span class={styles.path} title={props.path}>
          {props.path}
        </span>
        <div class={styles.toolbar}>
          <Show when={modelInfo()} keyed>
            {(info) => (
              <span
                class={styles.badge}
                title={`Dimensions: ${info.dimensions.x} × ${info.dimensions.y} × ${info.dimensions.z}`}
              >
                {info.triangleCount.toLocaleString()} triangles
              </span>
            )}
          </Show>
          <Show when={webglAvailable() && !loading() && !error()}>
            <button
              type="button"
              class={`${styles.toolBtn}${wireframe() ? ` ${styles.active}` : ""}`}
              onClick={toggleWireframe}
              title="Toggle wireframe display"
            >
              wireframe
            </button>
            <button type="button" class={styles.toolBtn} onClick={resetCamera} title="Reset camera position">
              reset view
            </button>
          </Show>
        </div>
        <a
          class={styles.rawLink}
          href={taskFileURL(props.taskId, props.path)}
          target="_blank"
          rel="noopener noreferrer"
        >
          raw file
        </a>
      </div>
      <div class={styles.content}>
        <Show when={error()}>
          {(err) => (
            <div class={styles.error} role="alert">
              {err()}
            </div>
          )}
        </Show>
        <Show when={loading() && !error()}>
          <div class={styles.loading}>Loading 3D model…</div>
        </Show>
        <Show when={!loading() && !error() && !webglAvailable()}>
          <div class={styles.notice}>3D canvas preview requires WebGL support in your browser.</div>
        </Show>
        <div
          ref={(el) => {
            wrapRef = el;
          }}
          class={styles.canvasWrap}
          classList={{ [styles.hidden]: loading() || Boolean(error()) || !webglAvailable() }}
        >
          <canvas
            ref={(el) => {
              canvasRef = el;
            }}
            class={styles.canvas}
          />
          <div class={styles.controlsHint}>Rotate: Left Drag | Pan: Right Drag | Zoom: Scroll</div>
        </div>
      </div>
    </div>
  );
}
