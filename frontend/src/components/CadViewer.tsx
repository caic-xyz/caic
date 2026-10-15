// CAD pane with cancellable STEP loading, depth translucency, wireframe, and icon display controls.

import { Show, createEffect, createSignal, onCleanup, onMount } from "solid-js";
import FileViewerHeader from "./FileViewerHeader";
import ActionButton from "./ActionButton";
import ResetIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";
import WireframeIcon from "@material-symbols/svg-400/outlined/view_in_ar.svg?solid";
import TranslucentIcon from "@material-symbols/svg-400/outlined/opacity.svg?solid";
import AxesIcon from "@material-symbols/svg-400/outlined/3d_rotation.svg?solid";
import CancelIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import RetryIcon from "@material-symbols/svg-400/outlined/refresh.svg?solid";

import styles from "./CadViewer.module.css";
import type { CadModelInfo, CadScene, CadViewMode } from "../cad";
import type { CadLoadProgress } from "../stepProtocol";

interface Props {
  taskId: string;
  taskPath: string;
  path: string;
}

export default function CadViewer(props: Props) {
  let retryRef: HTMLButtonElement | undefined;
  let canvasRef: HTMLCanvasElement | undefined;
  let wrapRef: HTMLDivElement | undefined;
  let scene: CadScene | null = null;
  let resizeObserver: ResizeObserver | null = null;
  let abortController: AbortController | null = null;

  const [loading, setLoading] = createSignal(true);
  const [cancelled, setCancelled] = createSignal(false);
  const [progress, setProgress] = createSignal<CadLoadProgress>({ stage: "download", completed: 0, total: null });
  const stageLabel = () =>
    ({
      download: "Downloading model",
      decompress: "Decompressing STEP",
      parse: "Parsing STEP",
      meshes: "Preparing meshes",
      render: "Preparing view",
    })[progress().stage];
  const progressValue = () => {
    const value = progress();
    return value.total !== null && value.total > 0 && value.completed !== null ? value.completed : undefined;
  };
  const progressText = () => {
    const value = progress();
    if (value.total !== null && value.total > 0 && value.completed !== null)
      return `${Math.floor((value.completed / value.total) * 100)}%`;
    return value.stage === "download" && value.completed !== null
      ? `${(value.completed / 1024 / 1024).toFixed(1)} MiB received`
      : "";
  };
  const [error, setError] = createSignal<string | null>(null);
  const [modelInfo, setModelInfo] = createSignal<CadModelInfo | null>(null);
  const [viewMode, setViewMode] = createSignal<CadViewMode>("solid");
  const [axesVisible, setAxesVisible] = createSignal(true);
  const [webglAvailable, setWebglAvailable] = createSignal(true);

  function toggleViewMode(mode: Exclude<CadViewMode, "solid">) {
    const next = viewMode() === mode ? "solid" : mode;
    setViewMode(next);
    scene?.setViewMode(next);
  }

  function resetCamera() {
    scene?.resetCamera();
  }

  function toggleAxes() {
    const next = !axesVisible();
    setAxesVisible(next);
    scene?.setAxesVisible(next);
  }

  function cancelLoad() {
    abortController?.abort();
    setLoading(false);
    setCancelled(true);
    queueMicrotask(() => retryRef?.focus());
  }

  async function loadModelData(taskId: string, filePath: string) {
    abortController?.abort();
    scene?.clearModel();
    const controller = new AbortController();
    const { signal } = controller;
    abortController = controller;
    setLoading(true);
    setCancelled(false);
    setError(null);
    setModelInfo(null);
    setProgress({ stage: "download", completed: 0, total: null });
    const report = (value: CadLoadProgress) => {
      if (!signal.aborted) setProgress(value);
    };
    try {
      const { createCadScene, isWebGLSupported, loadCadFile, computeModelInfo, disposeCadGroup } =
        await import("../cad");
      signal.throwIfAborted();
      const model = await loadCadFile(taskId, filePath, signal, report);
      const hasWebGL = isWebGLSupported();
      setWebglAvailable(hasWebGL);
      if (hasWebGL && canvasRef) {
        if (!scene) {
          scene = createCadScene(canvasRef);
          scene.setAxesVisible(axesVisible());
        }
        const info = await scene.loadModel(model, signal, report);
        signal.throwIfAborted();
        scene.setViewMode(viewMode());
        setModelInfo(info);
        if (wrapRef) {
          const rect = wrapRef.getBoundingClientRect();
          scene.resize(rect.width, rect.height);
        }
      } else {
        try {
          setModelInfo({ ...computeModelInfo(model.group), plates: model.plates });
        } finally {
          disposeCadGroup(model.group);
        }
      }
      setLoading(false);
    } catch (err: unknown) {
      if (signal.aborted) return;
      setLoading(false);
      setError(err instanceof Error ? err.message : "Failed to load 3D model");
    } finally {
      if (abortController === controller) abortController = null;
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
    const taskId = props.taskId;
    const path = props.path;
    void loadModelData(taskId, path);
  });

  return (
    <div class={styles.container}>
      <FileViewerHeader taskId={props.taskId} taskPath={props.taskPath} path={props.path}>
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
          <Show when={webglAvailable() && !loading() && !error() && !cancelled()}>
            <ActionButton
              label="wireframe"
              onClick={() => toggleViewMode("wireframe")}
              title="Toggle wireframe display"
              aria-pressed={viewMode() === "wireframe"}
            >
              <WireframeIcon width={20} height={20} />
            </ActionButton>
            <ActionButton
              label="Translucent"
              onClick={() => toggleViewMode("translucent")}
              title="Toggle depth-based translucency"
              aria-pressed={viewMode() === "translucent"}
            >
              <TranslucentIcon width={20} height={20} />
            </ActionButton>
            <ActionButton
              label="Axes"
              onClick={toggleAxes}
              title="Toggle XYZ reference frame"
              aria-pressed={axesVisible()}
            >
              <AxesIcon width={20} height={20} />
            </ActionButton>
            <ActionButton label="Reset camera" onClick={resetCamera} title="Reset camera position">
              <ResetIcon width={20} height={20} />
            </ActionButton>
          </Show>
        </div>
      </FileViewerHeader>
      <div class={styles.content}>
        <Show when={error() || cancelled()}>
          <div class={styles.loading}>
            <p classList={{ [styles.errorText]: Boolean(error()) }} role={error() ? "alert" : "status"}>
              {error() ?? "Model loading cancelled."}
            </p>
            <ActionButton ref={retryRef} label="Retry" onClick={() => void loadModelData(props.taskId, props.path)}>
              <RetryIcon width={20} height={20} />
            </ActionButton>
          </div>
        </Show>
        <Show when={loading()}>
          <div class={styles.loading} aria-busy="true">
            <p class={styles.loadingMessage} role="status">
              {stageLabel()}…
            </p>
            <Show
              when={progressValue() !== undefined}
              fallback={<progress class={styles.progress} aria-label={stageLabel()} />}
            >
              <progress
                class={styles.progress}
                aria-label={stageLabel()}
                max={progress().total ?? 1}
                value={progressValue() ?? 0}
              />
            </Show>
            <span class={styles.progressDetails}>{progressText()}</span>
            <ActionButton label="Cancel loading" onClick={cancelLoad}>
              <CancelIcon width={20} height={20} />
            </ActionButton>
          </div>
        </Show>
        <Show when={!loading() && !error() && !cancelled() && !webglAvailable()}>
          <div class={styles.notice}>3D canvas preview requires WebGL support in your browser.</div>
        </Show>
        <div
          ref={(el) => {
            wrapRef = el;
          }}
          class={styles.canvasWrap}
          classList={{ [styles.hidden]: loading() || cancelled() || Boolean(error()) || !webglAvailable() }}
        >
          <canvas
            ref={(el) => {
              canvasRef = el;
            }}
            class={styles.canvas}
            aria-label={axesVisible() ? "3D model with XYZ reference frame" : "3D model"}
          />
          <div class={styles.controlsHint}>Rotate: Left Drag | Pan: Right Drag | Zoom: Scroll</div>
        </div>
      </div>
    </div>
  );
}
