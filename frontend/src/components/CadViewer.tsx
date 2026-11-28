// CAD pane with staged loading and file-size feedback, error recovery, and display/axis controls.

import { Show, createEffect, createMemo, createSignal, onCleanup, onMount } from "solid-js";
import { Dynamic } from "solid-js/web";
import FileViewerHeader from "./FileViewerHeader";
import ActionButton from "./ActionButton";
import CadViewControls from "./CadViewControls";
import WireframeIcon from "@material-symbols/svg-400/outlined/view_in_ar.svg?solid";
import SolidIcon from "@material-symbols/svg-400/outlined/deployed_code.svg?solid";
import TranslucentIcon from "@material-symbols/svg-400/outlined/opacity.svg?solid";
import RetryIcon from "@material-symbols/svg-400/outlined/refresh.svg?solid";

import styles from "./CadViewer.module.css";
import type { CadModelInfo, CadScene, CadViewMode } from "../cad";
import { formatBytes } from "../formatting";
import { isStepPath } from "../runtimePath";
import { StepLoadTimings } from "../stepTimings";
import type { CadLoadProgress } from "../stepProtocol";

interface Props {
  scrollWithPage?: boolean;
  taskId: string;
  taskPath: string;
  path: string;
}

const VIEW_MODES = {
  solid: { label: "Solid", icon: SolidIcon, next: "wireframe" },
  wireframe: { label: "Wireframe", icon: WireframeIcon, next: "translucent" },
  translucent: { label: "Translucent", icon: TranslucentIcon, next: "solid" },
} as const;

export default function CadViewer(props: Props) {
  let canvasRef: HTMLCanvasElement | undefined;
  let wrapRef: HTMLDivElement | undefined;
  let contentRef: HTMLDivElement | undefined;
  let retryRef: HTMLButtonElement | undefined;
  let scene: CadScene | null = null;
  let resizeObserver: ResizeObserver | null = null;
  let abortController: AbortController | null = null;

  const [loading, setLoading] = createSignal(true);
  const [elapsedSeconds, setElapsedSeconds] = createSignal(0);
  const [slowImport, setSlowImport] = createSignal(false);
  const [progress, setProgress] = createSignal<CadLoadProgress>({ stage: "download", completed: 0, total: null });
  const [fileBytes, setFileBytes] = createSignal<number | null>(null);
  const fileSizeText = () => {
    const size = fileBytes();
    return size === null ? "" : `File size: ${formatBytes(size)}`;
  };
  const readingStep = createMemo(() => {
    const value = progress();
    return (
      value.stage === "download" &&
      value.decodedBytes !== undefined &&
      value.total !== null &&
      value.total > 0 &&
      value.completed === value.total
    );
  });
  const stageLabel = () => {
    const value = progress();
    if (value.stage === "download") {
      if (!value.completed) return "Loading model";
      if (readingStep()) return "Reading STEP";
      return value.decodedBytes === undefined ? "Downloading model" : "Downloading and reading STEP";
    }
    if (value.stage === "parse" && value.parseStep === "occt-init") return "Preparing STEP parser";
    if (value.stage === "parse" && value.parseStep === "geometry") return "Extracting STEP faces";
    if (value.stage === "parse" && value.parseStep === "input") return "Processing STEP";
    return {
      parse: "Parsing STEP",
      meshes: "Preparing meshes",
      render: "Preparing view",
    }[value.stage];
  };
  const progressValue = () => {
    const value = progress();
    return !readingStep() && value.total !== null && value.total > 0 && value.completed !== null
      ? value.completed
      : undefined;
  };
  const progressText = createMemo(() => {
    const value = progress();
    if (value.stage === "download") {
      if (!value.completed) return "";
      const details: string[] = [];
      if (!readingStep()) {
        if (value.total !== null && value.total > 0) {
          const percent = `${Math.floor((value.completed / value.total) * 100)}%`;
          details.push(value.decodedBytes === undefined ? percent : `${percent} received`);
        } else {
          details.push(`${formatBytes(value.completed)} received`);
        }
      }
      if (value.indexedRecords !== null && value.indexedRecords !== undefined) {
        details.push(`${value.indexedRecords.toLocaleString()} records indexed`);
      } else if (value.decodedBytes !== undefined) {
        details.push(`${formatBytes(value.decodedBytes)} decoded`);
      }
      return details.join(" · ");
    }
    if (value.total !== null && value.total > 0 && value.completed !== null) {
      if (value.stage === "parse")
        return value.parseStep === "geometry"
          ? `${value.completed.toLocaleString()} / ${value.total.toLocaleString()} faces`
          : `${(value.completed / 1024 / 1024).toFixed(1)} / ${(value.total / 1024 / 1024).toFixed(1)} MiB`;
      return `${Math.floor((value.completed / value.total) * 100)}%`;
    }
    return "";
  });
  const [error, setError] = createSignal<string | null>(null);
  const [modelInfo, setModelInfo] = createSignal<CadModelInfo | null>(null);
  const [viewMode, setViewMode] = createSignal<CadViewMode>("solid");
  const [axesVisible, setAxesVisible] = createSignal(true);
  const [webglAvailable, setWebglAvailable] = createSignal(true);

  const displayLabel = () => {
    const mode = VIEW_MODES[viewMode()];
    return `Display mode: ${mode.label}. Switch to ${VIEW_MODES[mode.next].label.toLowerCase()}`;
  };

  function cycleViewMode() {
    const next = VIEW_MODES[viewMode()].next;
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

  async function loadModelData(taskId: string, filePath: string) {
    const restoreRetryFocus = document.activeElement === retryRef;
    if (restoreRetryFocus) contentRef?.focus();
    abortController?.abort();
    scene?.clearModel();
    const controller = new AbortController();
    const { signal } = controller;
    const timings = isStepPath(filePath) ? new StepLoadTimings() : undefined;
    abortController = controller;
    setLoading(true);
    setError(null);
    setModelInfo(null);
    setProgress({ stage: "download", completed: 0, total: null });
    setFileBytes(null);
    setElapsedSeconds(0);
    setSlowImport(false);
    const started = performance.now();
    let occtStarted: number | null = null;
    // The kernel runs synchronously in its worker. Own its clock here so elapsed
    // feedback remains available without worker messages. A slow
    // hint starts after ten seconds inside OCCT, excluding parser initialization.
    const timer = timings
      ? setInterval(() => {
          if (signal.aborted || abortController !== controller) return;
          const now = performance.now();
          setElapsedSeconds(Math.floor((now - started) / 1000));
          setSlowImport(occtStarted !== null && now - occtStarted >= 10_000);
        }, 1000)
      : null;
    const stopTimer = () => {
      if (timer !== null) clearInterval(timer);
    };
    signal.addEventListener("abort", stopTimer, { once: true });
    const report = (value: CadLoadProgress) => {
      if (signal.aborted) return;
      if (value.stage === "parse" && value.parseStep === "occt") {
        occtStarted ??= performance.now();
      } else {
        occtStarted = null;
        setSlowImport(false);
      }
      if (value.stage === "download") setFileBytes(value.total);
      else if (value.fileBytes !== undefined) setFileBytes(value.fileBytes);
      setProgress(value);
    };
    try {
      const { createCadScene, isWebGLSupported, loadCadFile, computeModelInfo, disposeCadGroup } =
        await import("../cad");
      signal.throwIfAborted();
      const model = await loadCadFile(taskId, filePath, signal, report, timings);
      signal.throwIfAborted();
      const sceneStarted = performance.now();
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
        if (timings) timings.sceneSetupMs = performance.now() - sceneStarted;
        setLoading(false);
        const frameStarted = performance.now();
        if (wrapRef) {
          const rect = wrapRef.getBoundingClientRect();
          scene.resize(rect.width, rect.height);
        }
        await scene.whenRendered(signal);
        signal.throwIfAborted();
        if (timings) {
          timings.firstFrameMs = performance.now() - frameStarted;
          timings.report();
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
      if (restoreRetryFocus && document.activeElement === contentRef) retryRef?.focus();
    } finally {
      stopTimer();
      signal.removeEventListener("abort", stopTimer);
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
    <div class={`${styles.container} ${props.scrollWithPage ? styles.scrolling : ""}`}>
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
          <Show when={webglAvailable() && !loading() && !error()}>
            <ActionButton label={displayLabel()} onClick={cycleViewMode}>
              <Dynamic component={VIEW_MODES[viewMode()].icon} width={20} height={20} aria-hidden="true" />
            </ActionButton>
          </Show>
        </div>
      </FileViewerHeader>
      <div class={styles.content} ref={contentRef} role="region" aria-label="Model viewer" tabIndex={-1}>
        <Show when={error()}>
          <div class={styles.loading}>
            <p class={styles.errorText} role="alert">
              {error()}
            </p>
            <ActionButton ref={retryRef} label="Retry" onClick={() => void loadModelData(props.taskId, props.path)}>
              <RetryIcon width={20} height={20} />
              Retry
            </ActionButton>
          </div>
        </Show>
        <Show when={loading()}>
          <div class={styles.loading}>
            <div class={styles.loadingProgress}>
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
              <div class={styles.progressDetails}>
                <span>{fileSizeText()}</span>
                {fileBytes() !== null && progressText() && " · "}
                <span>{progressText()}</span>
              </div>
            </div>
            <Show when={isStepPath(props.path)}>
              <div class={styles.loadFeedback}>
                <span class={styles.progressDetails}>Elapsed: {elapsedSeconds()} s</span>
                <p class={styles.slowMessage} role="status">
                  {slowImport() ? "This model is taking longer to parse." : ""}
                </p>
              </div>
            </Show>
          </div>
        </Show>
        <Show when={!loading() && !error() && !webglAvailable()}>
          <div class={styles.notice}>3D canvas preview requires WebGL support in your browser.</div>
        </Show>
        <div
          ref={(el) => {
            wrapRef = el;
          }}
          class={`${styles.canvasWrap} ${loading() || Boolean(error()) || !webglAvailable() ? styles.hidden : ""}`}
        >
          <CadViewControls axesVisible={axesVisible()} onToggleAxes={toggleAxes} onResetView={resetCamera}>
            <canvas
              ref={(el) => {
                canvasRef = el;
              }}
              class={styles.canvas}
              aria-label={axesVisible() ? "3D model with XYZ reference frame" : "3D model"}
            />
            <div class={styles.controlsHint}>Rotate: Left Drag | Pan: Right Drag | Zoom: Scroll</div>
          </CadViewControls>
        </div>
      </div>
    </div>
  );
}
