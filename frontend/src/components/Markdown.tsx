// Markdown renders agent and file text with runtime file links, in-app markdown and CAD navigation, copyable code, inline mermaid diagrams, and a raw toggle.

import {
  Show,
  createContext,
  createEffect,
  createMemo,
  createSignal,
  onCleanup,
  onMount,
  useContext,
  type Accessor,
} from "solid-js";
import { useNavigate } from "@solidjs/router";
import { Marked, Renderer, type Tokens } from "marked";
import CopyIcon from "@material-symbols/svg-400/outlined/content_copy.svg?solid";
import CheckIcon from "@material-symbols/svg-400/outlined/check.svg?solid";

import styles from "./Markdown.module.css";
import { isViewablePath, runtimeFilePath, taskFileURL, taskViewURL } from "../runtimePath";
import { getMermaidCachedSvg, renderMermaidBlocks, toggleMermaidView } from "../mermaid";

/** The task whose runtime files markdown paths name. Without it, links stay ordinary. */
export const MarkdownTaskContext = createContext<Accessor<string>>();

const markdownRenderer = {
  code(this: Renderer, token: Tokens.Code): string {
    const code = Renderer.prototype.code.call(this, token);
    if (token.codeBlockStyle === "indented") return code;
    const blockClass = token.text.includes("\n")
      ? styles.codeBlock
      : `${styles.codeBlock} ${styles.singleLineCodeBlock}`;

    if (token.lang === "mermaid") {
      const cached = getMermaidCachedSvg(token.text);
      if (cached !== null) {
        return `<div class="${styles.mermaidBlock}" data-mermaid-block><div class="${styles.mermaidToolbar}"><button type="button" class="${styles.mermaidToggleBtn}" data-mermaid-toggle aria-label="Show code" title="Show code">code</button><button type="button" class="${styles.codeCopyBtn}" data-copy-code aria-label="Copy code block" title="Copy code block"><span class="${styles.copyIcon}" aria-hidden="true"></span><span class="${styles.checkIcon}" aria-hidden="true"></span></button></div><div class="${styles.mermaidDiagram}" data-mermaid-diagram role="img" aria-label="Mermaid diagram">${cached}</div><div class="${styles.mermaidSource}" data-mermaid-source style="display: none;"><div class="${blockClass}">${code}</div></div></div>\n`;
      }

      return `<div class="${styles.mermaidBlock}" data-mermaid-block><div class="${styles.mermaidToolbar}"><button type="button" class="${styles.mermaidToggleBtn}" data-mermaid-toggle aria-label="Show code" title="Show code" style="display: none;">code</button><button type="button" class="${styles.codeCopyBtn}" data-copy-code aria-label="Copy code block" title="Copy code block"><span class="${styles.copyIcon}" aria-hidden="true"></span><span class="${styles.checkIcon}" aria-hidden="true"></span></button></div><div class="${styles.mermaidDiagram}" data-mermaid-diagram role="img" aria-label="Mermaid diagram"><div class="${styles.mermaidPlaceholder}"></div></div><div class="${styles.mermaidSource}" data-mermaid-source style="display: none;"><div class="${blockClass}">${code}</div></div></div>\n`;
    }

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
        // In-app viewable files (markdown, STL, STEP) open in the app; other files open in a new tab.
        if (isViewablePath(path)) {
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
    const toggleBtn = target.closest<HTMLButtonElement>("button[data-mermaid-toggle]");
    if (toggleBtn) {
      toggleMermaidView(toggleBtn);
      return;
    }
    copyCodeBlock(target);
  }

  function copyCodeBlock(target: Element) {
    const button = target.closest<HTMLButtonElement>("button[data-copy-code]");
    if (!button) return;

    const block = button.closest(`.${styles.mermaidBlock}`) ?? button.parentElement;
    const text = block?.querySelector("pre > code")?.textContent;
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

  createEffect(() => {
    const currentHtml = html();
    const isRaw = raw();
    if (isRaw || !wrapperRef) return;
    // Track currentHtml so effect re-runs when marked output updates
    void currentHtml;
    void renderMermaidBlocks(wrapperRef);
  });

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
