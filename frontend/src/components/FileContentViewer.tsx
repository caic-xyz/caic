// Renders task-runtime Markdown, inert images, bounded plain text, and unsupported-file downloads.

import { Match, Show, Switch, createMemo, createResource, createSignal, onCleanup, onMount } from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";

import styles from "./FileContentViewer.module.css";
import Markdown, { MarkdownTaskContext } from "./Markdown";
import { dirname, isMarkdownPath, taskRawURL } from "../runtimePath";
import { splitFrontMatter } from "../frontMatter";
import { readTaskFilePreview } from "../taskFile";

interface Props {
  taskId: string;
  taskPath: string;
  path: string;
}

export default function FileContentViewer(props: Props) {
  const navigate = useNavigate();
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
        <a class={styles.rawLink} href={taskRawURL(props.taskId, props.path)} target="_blank" rel="noopener noreferrer">
          raw file
        </a>
      </div>
      <div class={styles.content}>
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
              <img
                class={styles.image}
                src={taskRawURL(props.taskId, props.path)}
                alt={props.path}
                onError={() => setImageError(true)}
              />
            </Show>
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
