// Renders task-runtime directories, Markdown, images, native audio/video, lazy PDFs, bounded text, and unsupported downloads.

import {
  For,
  Match,
  Show,
  Suspense,
  Switch,
  createMemo,
  createResource,
  createSignal,
  lazy,
  onCleanup,
} from "solid-js";
import { A } from "@solidjs/router";
import { Dynamic } from "solid-js/web";
import FolderIcon from "@material-symbols/svg-400/outlined/folder.svg?solid";
import FileIcon from "@material-symbols/svg-400/outlined/draft.svg?solid";
import ImagePreview from "./ImagePreview";
import FileViewerHeader from "./FileViewerHeader";

import styles from "./FileContentViewer.module.css";
import Markdown, { MarkdownTaskContext } from "./Markdown";
import { dirname, isMarkdownPath, joinRuntimePath, taskRawURL, taskViewURL } from "../runtimePath";
import { splitFrontMatter } from "../frontMatter";
import { formatBytes } from "../formatting";
import { readTaskFilePreview, type TaskFilePreview } from "../taskFile";

const PdfPreview = lazy(() => import("./PdfPreview"));

interface Props {
  taskId: string;
  taskPath: string;
  path: string;
  line?: number | null;
}

// SourceText reveals and marks the line a link names. Splitting the text into
// three parts keeps one element instead of one per line.
function SourceText(props: { text: string; line: number | null }) {
  const split = createMemo(() => {
    if (props.line === null) return null;
    const lines = props.text.split("\n");
    if (props.line > lines.length) return null;
    // The newline boundaries live in the strings: JSX drops whitespace-only children.
    return {
      before: lines.slice(0, props.line - 1).join("\n") + (props.line > 1 ? "\n" : ""),
      target: lines[props.line - 1] ?? "",
      after: (props.line < lines.length ? "\n" : "") + lines.slice(props.line).join("\n"),
    };
  });
  return (
    <Show when={split()} keyed fallback={<pre class={styles.source}>{props.text}</pre>}>
      {(parts) => (
        <pre class={styles.source}>
          {parts.before}
          <mark class={styles.sourceLine} ref={(el: HTMLElement) => el.scrollIntoView({ block: "center" })}>
            {parts.target}
          </mark>
          {parts.after}
        </pre>
      )}
    </Show>
  );
}

// DirListing opens entries through the same viewer route, including the parent.
function DirListing(props: { taskId: string; listing: Extract<TaskFilePreview, { kind: "dir" }> }) {
  return (
    <div class={styles.dirListing}>
      <ul class={styles.dirEntries}>
        <Show when={props.listing.path !== "/"}>
          <li>
            <A
              class={styles.dirEntry}
              href={taskViewURL(props.taskId, dirname(props.listing.path))}
              aria-label="Parent directory"
            >
              <span class={styles.dirEntryName} aria-hidden="true">
                ..
              </span>
            </A>
          </li>
        </Show>
        <For each={props.listing.entries}>
          {(entry) => (
            <li>
              <A
                class={styles.dirEntry}
                href={taskViewURL(props.taskId, joinRuntimePath(props.listing.path, entry.name))}
                title={
                  entry.modified ? `${entry.name}, modified ${new Date(entry.modified).toLocaleString()}` : entry.name
                }
              >
                <Show when={entry.isDir} fallback={<FileIcon width="1.1em" height="1.1em" aria-hidden="true" />}>
                  <FolderIcon width="1.1em" height="1.1em" aria-hidden="true" />
                </Show>
                <span class={styles.dirEntryName}>{entry.isDir ? `${entry.name}/` : entry.name}</span>
                <Show when={!entry.isDir}>
                  <span class={styles.dirEntryMeta} aria-hidden="true">
                    {formatBytes(entry.size)}
                  </span>
                </Show>
              </A>
            </li>
          )}
        </For>
      </ul>
      <Show when={props.listing.entries.length === 0}>
        <div class={styles.notice}>This directory is empty.</div>
      </Show>
      <Show when={props.listing.truncated}>
        <div class={styles.notice}>Showing the first {props.listing.entries.length} entries only.</div>
      </Show>
    </div>
  );
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
  const dirListing = createMemo(() => {
    const preview = file.state === "ready" ? file() : null;
    return preview?.kind === "dir" ? preview : null;
  });

  return (
    <div class={styles.container}>
      <FileViewerHeader
        taskId={props.taskId}
        taskPath={props.taskPath}
        path={props.path}
        directory={dirListing() !== null}
      />
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
          <Match when={dirListing()} keyed>
            {(listing) => <DirListing taskId={props.taskId} listing={listing} />}
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
          <Match when={(props.line !== null || !isMarkdownPath(props.path)) && textFile()} keyed>
            {(source) => (
              <>
                <Show when={isMarkdownPath(props.path)}>
                  <div class={styles.notice}>Source view: the link names line {props.line}.</div>
                </Show>
                <SourceText text={source.text} line={props.line ?? null} />
                <Show when={source.truncated}>
                  <div class={styles.notice}>File truncated: only the first 1 MiB is shown.</div>
                </Show>
              </>
            )}
          </Match>
          <Match when={props.line === null && isMarkdownPath(props.path) && textFile()}>
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
