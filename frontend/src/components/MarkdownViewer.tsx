// Full-page viewer that renders a markdown file from a task runtime, with links resolved against its directory.

import { Match, Show, Switch, createMemo, createResource, onCleanup, onMount } from "solid-js";
import { useNavigate } from "@solidjs/router";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";

import styles from "./MarkdownViewer.module.css";
import Markdown, { MarkdownTaskContext } from "./Markdown";
import { dirname, taskFileURL } from "../runtimePath";
import { splitFrontMatter } from "../frontMatter";
import { readTaskTextFile } from "../taskFile";

interface Props {
  taskId: string;
  taskPath: string;
  path: string;
}

export default function MarkdownViewer(props: Props) {
  const navigate = useNavigate();
  let abort: AbortController | undefined;
  onCleanup(() => abort?.abort());
  const [file] = createResource(
    () => ({ taskId: props.taskId, path: props.path }),
    (source) => {
      abort?.abort();
      abort = new AbortController();
      return readTaskTextFile(source.taskId, source.path, abort.signal);
    },
  );
  const taskId = createMemo(() => props.taskId);
  const doc = createMemo(() => splitFrontMatter(file.state === "ready" ? (file()?.text ?? "") : ""));

  // Escape navigates back to the task detail.
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") navigate(props.taskPath);
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button class={styles.backBtn} onClick={() => navigate(props.taskPath)} title="Back to task">
          <ArrowBackIcon width={20} height={20} />
        </button>
        <span class={styles.path} title={props.path}>
          {props.path}
        </span>
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
        <Switch>
          <Match when={file.state === "errored"}>
            <div class={styles.error} role="alert">
              {file.error instanceof Error ? file.error.message : "Failed to load file"}
            </div>
          </Match>
          <Match when={file.state === "ready"}>
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
                <Show when={file()?.truncated}>
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
