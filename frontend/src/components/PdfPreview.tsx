// Previews PDFs one page at a time with selectable text, URL-backed navigation, zoom, and cancelled rendering.
import { For, Show, createEffect, createMemo, createSignal, onCleanup, onMount } from "solid-js";
import { useSearchParams } from "@solidjs/router";
import { TextLayer, type PDFDocumentProxy, type RenderTask } from "pdfjs-dist";
import PreviousIcon from "@material-symbols/svg-400/outlined/chevron_left.svg?solid";
import NextIcon from "@material-symbols/svg-400/outlined/chevron_right.svg?solid";
import ActionButton from "./ActionButton";
import { loadPdf } from "../pdf";
import { taskRawURL } from "../runtimePath";
import styles from "./PdfPreview.module.css";

const zoomLevels = [0.5, 1, 1.5, 2, 3];
// Limit the backing canvas to 16 MiB even on high-DPI screens or oversized pages.
const maxCanvasPixels = 4_194_304;

export default function PdfPreview(props: { taskId: string; path: string }) {
  let viewport!: HTMLDivElement;
  let pageHost!: HTMLDivElement;
  const [query, setQuery] = useSearchParams();
  const [pdf, setPdf] = createSignal<PDFDocumentProxy | null>(null);
  const [width, setWidth] = createSignal(0);
  const [busy, setBusy] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  const pageNumber = createMemo(() => {
    const value = Number(query.pdfPage);
    return Number.isInteger(value) && value >= 1 && value <= (pdf()?.numPages ?? 0) ? value : 1;
  });
  const zoom = createMemo(() => {
    const value = Number(query.pdfZoom);
    return zoomLevels.includes(value) ? value : null;
  });

  onMount(() => {
    const observer = new ResizeObserver(() => setWidth(viewport.clientWidth));
    observer.observe(viewport);
    setWidth(viewport.clientWidth);
    onCleanup(() => observer.disconnect());
  });

  createEffect(() => {
    const abort = new AbortController();
    const loading = loadPdf(taskRawURL(props.taskId, props.path), abort.signal);
    let disposed = false;
    setPdf(null);
    setError(null);
    setBusy(true);
    void loading.promise.then(
      (doc) => {
        if (!disposed) setPdf(doc);
      },
      (err: unknown) => {
        if (!disposed) setError(err instanceof Error ? err.message : "Failed to load PDF");
      },
    );
    onCleanup(() => {
      disposed = true;
      abort.abort();
      void loading.destroy().catch((err: unknown) => console.error("Failed to close PDF", err));
    });
  });

  createEffect(() => {
    const doc = pdf();
    const number = pageNumber();
    const availableWidth = width();
    const scale = zoom();
    if (!doc || availableWidth <= 0) return;
    let disposed = false;
    let render: RenderTask | null = null;
    let text: TextLayer | null = null;
    setBusy(true);
    setError(null);
    onCleanup(() => {
      disposed = true;
      render?.cancel();
      text?.cancel();
    });
    void (async () => {
      const page = await doc.getPage(number);
      try {
        if (disposed) return;
        const original = page.getViewport({ scale: 1 });
        const pageViewport = page.getViewport({ scale: scale ?? availableWidth / original.width });
        const ratio = Math.min(
          window.devicePixelRatio || 1,
          Math.sqrt(maxCanvasPixels / (pageViewport.width * pageViewport.height)),
        );
        const canvas = document.createElement("canvas");
        canvas.width = Math.max(1, Math.floor(pageViewport.width * ratio));
        canvas.height = Math.max(1, Math.floor(pageViewport.height * ratio));
        canvas.className = `${styles.canvas}`;
        canvas.setAttribute("aria-hidden", "true");
        const layer = document.createElement("div");
        layer.className = `${styles.textLayer}`;
        const container = document.createElement("div");
        container.className = `${styles.page}`;
        container.style.setProperty("--pdf-width", `${pageViewport.width}px`);
        container.style.setProperty("--pdf-height", `${pageViewport.height}px`);
        container.style.setProperty("--total-scale-factor", String(pageViewport.scale));
        container.append(canvas, layer);
        render = page.render({ canvas, viewport: pageViewport, transform: [ratio, 0, 0, ratio, 0, 0] });
        await render.promise;
        if (disposed) return;
        text = new TextLayer({ textContentSource: page.streamTextContent(), container: layer, viewport: pageViewport });
        await text.render();
        if (disposed) return;
        pageHost.replaceChildren(container);
        viewport.scrollTo(0, 0);
        setBusy(false);
      } finally {
        page.cleanup();
      }
    })().catch((err: unknown) => {
      if (!disposed) {
        setError(err instanceof Error ? err.message : "Failed to render PDF page");
        setBusy(false);
      }
    });
  });

  function goToPage(number: number) {
    setQuery({ pdfPage: number === 1 ? undefined : String(number) }, { replace: false });
  }

  return (
    <div class={styles.container}>
      <Show when={pdf()} keyed>
        {(doc) => (
          <div class={styles.toolbar} role="group" aria-label="PDF controls">
            <ActionButton
              label="Previous page"
              aria-disabled={pageNumber() === 1}
              onClick={() => {
                if (pageNumber() > 1) goToPage(pageNumber() - 1);
              }}
            >
              <PreviousIcon width={20} height={20} />
            </ActionButton>
            <span class={styles.pageNumber} aria-live="polite">
              Page {pageNumber()} of {doc.numPages}
            </span>
            <ActionButton
              label="Next page"
              aria-disabled={pageNumber() === doc.numPages}
              onClick={() => {
                if (pageNumber() < doc.numPages) goToPage(pageNumber() + 1);
              }}
            >
              <NextIcon width={20} height={20} />
            </ActionButton>
            <select
              class={styles.zoom}
              aria-label="PDF zoom"
              value={zoom() ?? "fit"}
              onChange={(event) =>
                setQuery(
                  { pdfZoom: event.currentTarget.value === "fit" ? undefined : event.currentTarget.value },
                  { replace: false },
                )
              }
            >
              <option value="fit">Fit width</option>
              <For each={zoomLevels}>{(level) => <option value={level}>{level * 100}%</option>}</For>
            </select>
          </div>
        )}
      </Show>
      <Show when={error()}>
        {(message) => (
          <div class={styles.error} role="alert">
            {message()}
          </div>
        )}
      </Show>
      <Show when={busy() && !error()}>
        <div class={styles.loading} role="status">
          Loading PDF…
        </div>
      </Show>
      <div ref={viewport} class={styles.viewport} tabindex="0" role="region" aria-label="PDF page" aria-busy={busy()}>
        <div ref={pageHost} class={`${styles.pageHost} ${busy() || Boolean(error()) ? styles.hidden : ""}`} />
      </div>
    </div>
  );
}
