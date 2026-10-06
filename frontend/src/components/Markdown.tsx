// Markdown renders agent and file text with runtime file links, in-app markdown navigation, copyable code, and a raw toggle.

import { Show, createContext, createMemo, createSignal, onCleanup, onMount, useContext, type Accessor } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { Marked, Renderer, type Tokens } from "marked";
import CopyIcon from "@material-symbols/svg-400/outlined/content_copy.svg?solid";
import CheckIcon from "@material-symbols/svg-400/outlined/check.svg?solid";

import styles from "./Markdown.module.css";
import { isMarkdownPath, runtimeFilePath, taskFileURL, taskViewURL } from "../runtimePath";

/** The task whose runtime files markdown paths name. Without it, links stay ordinary. */
export const MarkdownTaskContext = createContext<Accessor<string>>();

const markdownRenderer = {
  code(this: Renderer, token: Tokens.Code): string {
    const code = Renderer.prototype.code.call(this, token);
    if (token.codeBlockStyle === "indented") return code;
    const blockClass = token.text.includes("\n")
      ? styles.codeBlock
      : `${styles.codeBlock} ${styles.singleLineCodeBlock}`;
    return `<div class="${blockClass}"><button type="button" class="${styles.codeCopyBtn}" data-copy-code aria-label="Copy code block" title="Copy code block"><span class="${styles.copyIcon}" aria-hidden="true"></span><span class="${styles.checkIcon}" aria-hidden="true"></span></button>${code}</div>\n`;
  },
};

/**
 * baseDir is the runtime directory of the rendered document. Relative links
 * and images resolve against it; without it the task checkout owns them.
 */
export default function Markdown(props: { text: string; baseDir?: string }) {
  const taskId = useContext(MarkdownTaskContext);
  const navigate = useNavigate();
  const runtimePath = (href: string) => (taskId ? runtimeFilePath(href, props.baseDir ?? null) : null);
  const marked = new Marked({
    breaks: true,
    gfm: true,
    renderer: {
      ...markdownRenderer,
      link(this: Renderer, token: Tokens.Link): string {
        const path = runtimePath(token.href);
        if (path === null || !taskId) return Renderer.prototype.link.call(this, token);
        // Markdown documents open in the app; other runtime files open in a new tab.
        if (isMarkdownPath(path)) {
          return Renderer.prototype.link
            .call(this, { ...token, href: taskViewURL(taskId(), path) })
            .replace("<a ", "<a data-app-link ");
        }
        return Renderer.prototype.link
          .call(this, { ...token, href: taskFileURL(taskId(), path) })
          .replace("<a ", '<a target="_blank" rel="noopener noreferrer" ');
      },
      image(this: Renderer, token: Tokens.Image): string {
        const path = runtimePath(token.href);
        if (path === null || !taskId) return Renderer.prototype.image.call(this, token);
        return Renderer.prototype.image.call(this, { ...token, href: taskFileURL(taskId(), path) });
      },
    },
  });
  const html = createMemo(() => marked.parse(props.text) as string);
  const [raw, setRaw] = createSignal(false);
  const [copied, setCopied] = createSignal(false);
  let wrapperRef: HTMLDivElement | undefined;

  function onWrapperClick(event: MouseEvent) {
    const target = event.target;
    if (!(target instanceof Element)) return;
    const link = target.closest<HTMLAnchorElement>("a[data-app-link]");
    // Modified clicks keep the browser's open-in-new-tab behavior.
    if (link && event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) {
      event.preventDefault();
      navigate(link.getAttribute("href") ?? "");
      return;
    }
    copyCodeBlock(target);
  }

  function copyCodeBlock(target: Element) {
    const button = target.closest<HTMLButtonElement>("button[data-copy-code]");
    if (!button) return;

    const text = button.parentElement?.querySelector("pre > code")?.textContent;
    if (text === undefined || text === null) return;

    navigator.clipboard
      .writeText(text)
      .then(() => {
        button.classList.add(styles.copied);
        setTimeout(() => {
          button.classList.remove(styles.copied);
        }, 1500);
      })
      .catch((error: unknown) => {
        console.error("Failed to copy code block", error);
      });
  }

  onMount(() => {
    const wrapper = wrapperRef;
    if (!wrapper) return;
    wrapper.addEventListener("click", onWrapperClick);
    onCleanup(() => wrapper.removeEventListener("click", onWrapperClick));
  });

  return (
    <div
      class={styles.markdownWrap}
      data-markdown-wrap
      ref={(el) => {
        wrapperRef = el;
      }}
    >
      <div class={styles.rawToolbar}>
        <button
          class={styles.rawToolbarBtn}
          onClick={() => setRaw(!raw())}
          title={raw() ? "Show rendered" : "Show raw"}
        >
          {raw() ? "rendered" : "raw"}
        </button>
        <Show when={raw()}>
          <button
            class={`${styles.rawCopyBtn} ${copied() ? styles.copied : ""}`}
            onClick={() => {
              navigator.clipboard.writeText(props.text);
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            }}
            title="Copy to clipboard"
          >
            <CopyIcon width={14} height={14} class={styles.copyIcon} />
            <CheckIcon width={14} height={14} class={styles.checkIcon} />
          </button>
        </Show>
      </div>
      <Show
        when={raw()}
        fallback={
          // eslint-disable-next-line solid/no-innerhtml -- rendering trusted marked output
          <div class={styles.markdown} innerHTML={html()} />
        }
      >
        <pre class={styles.rawText}>{props.text}</pre>
      </Show>
    </div>
  );
}
