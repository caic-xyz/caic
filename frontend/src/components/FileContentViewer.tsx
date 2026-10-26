// Renders task-runtime Markdown, images, native audio/video, lazy PDFs, bounded text, and unsupported downloads.

import { Match, Show, Suspense, Switch, createMemo, createResource, createSignal, lazy, onCleanup } from "solid-js";
import { Dynamic } from "solid-js/web";
import ImagePreview from "./ImagePreview";
import FileViewerHeader from "./FileViewerHeader";

import styles from "./FileContentViewer.module.css";
import Markdown, { MarkdownTaskContext } from "./Markdown";
import { dirname, isMarkdownPath, taskRawURL } from "../runtimePath";
import { splitFrontMatter } from "../frontMatter";
import { readTaskFilePreview } from "../taskFile";

const PdfPreview = lazy(() => import("./PdfPreview"));

interface Props {
  taskId: string;
  taskPath: string;
  path: string;
}

function MediaPreview(props: { taskId: string; path: string; kind: "audio" | "video" }) {
  let media!: HTMLMediaElement;
  const [failed, setFailed] = createSignal(false);
  onCleanup(() => {
    media.pause();
    media.removeAttribute("src");
    media.load();
  });
  return (
    <div class={styles.mediaPreview}>
      <Dynamic
        component={props.kind}
        ref={(el: HTMLMediaElement) => {
          media = el;
        }}
        class={props.kind === "audio" ? styles.audio : styles.video}
        aria-label={props.path}
        controls
        autoplay
        playsinline
        src={taskRawURL(props.taskId, props.path)}
        onError={() => setFailed(true)}
      />
      <Show when={failed()}>
        <div class={styles.error} role="alert">
          Unable to play this {props.kind} file. Open the raw file to download it.
        </div>
      </Show>
    </div>
  );
}

export default function FileContentViewer(props: Props) {
  let abort: AbortController | undefined;
  const [imageError, setImageError] = createSignal(false);
  onCleanup(() => abort?.abort());
  const [file] = createResource(
    () => ({ taskId: props.taskId, path: props.path }),
    (source) => {
      abort?.abort();
      setImageError(false);
      abort = new AbortController();
      return readTaskFilePreview(source.taskId, source.path, abort.signal);
    },
  );
  const taskId = createMemo(() => props.taskId);
  const textFile = createMemo(() => {
    const preview = file.state === "ready" ? file() : null;
    return preview?.kind === "text" ? preview : null;
  });
  const doc = createMemo(() => splitFrontMatter(textFile()?.text ?? ""));

  return (
    <div class={styles.container}>
      <FileViewerHeader taskId={props.taskId} taskPath={props.taskPath} path={props.path} />
      <div class={`${styles.content} ${file.state === "ready" && file()?.kind === "pdf" ? styles.pdfContent : ""}`}>
        <Switch>
          <Match when={file.state === "errored"}>
            <div class={styles.error} role="alert">
              {file.error instanceof Error ? file.error.message : "Failed to load file"}
            </div>
          </Match>
          <Match when={file.state === "ready" && file()?.kind === "image"}>
            <Show
              when={!imageError()}
              fallback={
                <div class={styles.error} role="alert">
                  Failed to load image
                </div>
              }
            >
              <ImagePreview
                src={taskRawURL(props.taskId, props.path)}
                fullSrc={taskRawURL(props.taskId, props.path)}
                alt={props.path}
                onError={() => setImageError(true)}
              />
            </Show>
          </Match>
          <Match when={file.state === "ready" && file()?.kind === "pdf"}>
            <Suspense fallback={<div class={styles.loading}>Loading PDF…</div>}>
              <PdfPreview taskId={props.taskId} path={props.path} />
            </Suspense>
          </Match>
          <Match when={file.state === "ready" && file()?.kind === "audio"}>
            <MediaPreview taskId={props.taskId} path={props.path} kind="audio" />
          </Match>
          <Match when={file.state === "ready" && file()?.kind === "video"}>
            <MediaPreview taskId={props.taskId} path={props.path} kind="video" />
          </Match>
          <Match when={file.state === "ready" && file()?.kind === "unsupported"}>
            <div class={styles.notice}>No preview for this file. Open the raw file to download it.</div>
          </Match>
          <Match when={textFile() && !isMarkdownPath(props.path)}>
            <pre class={styles.source}>{textFile()?.text}</pre>
            <Show when={textFile()?.truncated}>
              <div class={styles.notice}>File truncated: only the first 1 MiB is shown.</div>
            </Show>
          </Match>
          <Match when={textFile()}>
            <MarkdownTaskContext.Provider value={taskId}>
              <article class={styles.page}>
                <Show when={doc().frontMatter} keyed>
                  {(frontMatter) => (
                    <details class={styles.frontMatter}>
                      <summary>Front matter ({frontMatter.format})</summary>
                      <pre class={styles.frontMatterSource}>{frontMatter.source}</pre>
                    </details>
                  )}
                </Show>
                <Markdown text={doc().body} baseDir={dirname(props.path)} />
                <Show when={textFile()?.truncated}>
                  <div class={styles.notice}>File truncated: only the first 1 MiB is shown.</div>
                </Show>
              </article>
            </MarkdownTaskContext.Provider>
          </Match>
          <Match when={true}>
            <div class={styles.loading}>Loading…</div>
          </Match>
        </Switch>
      </div>
    </div>
  );
}
