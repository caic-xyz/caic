// Markdown safely renders agent and file text with viewer links, space-tolerant paths, copyable code, Pele diagrams, and a raw toggle.

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
import { Marked, Renderer, Tokenizer, type Tokens } from "marked";
import DOMPurify from "dompurify";
import CopyIcon from "@material-symbols/svg-400/outlined/content_copy.svg?solid";
import CheckIcon from "@material-symbols/svg-400/outlined/check.svg?solid";

import styles from "./Markdown.module.css";
import { runtimeFilePath, runtimeFileTarget, taskRawURL, taskViewURL } from "../runtimePath";
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
        // renderMermaidBlocks inserts sanitized diagram SVG after Markdown sanitization.
        return `<div class="${styles.mermaidBlock}" data-mermaid-block><div class="${styles.mermaidToolbar}"><button type="button" class="${styles.mermaidToggleBtn}" data-mermaid-toggle aria-label="Show code" title="Show code">code</button><button type="button" class="${styles.codeCopyBtn}" data-copy-code aria-label="Copy code block" title="Copy code block"><span class="${styles.copyIcon}" aria-hidden="true"></span><span class="${styles.checkIcon}" aria-hidden="true"></span></button></div><div class="${styles.mermaidDiagram}" data-mermaid-diagram role="img" aria-label="Mermaid diagram"></div><div class="${styles.mermaidSource}" data-mermaid-source style="display: none;"><div class="${blockClass}">${code}</div></div></div>\n`;
      }

      return `<div class="${styles.mermaidBlock}" data-mermaid-block><div class="${styles.mermaidToolbar}"><button type="button" class="${styles.mermaidToggleBtn}" data-mermaid-toggle aria-label="Show code" title="Show code" style="display: none;">code</button><button type="button" class="${styles.codeCopyBtn}" data-copy-code aria-label="Copy code block" title="Copy code block"><span class="${styles.copyIcon}" aria-hidden="true"></span><span class="${styles.checkIcon}" aria-hidden="true"></span></button></div><div class="${styles.mermaidDiagram}" data-mermaid-diagram role="img" aria-label="Mermaid diagram"><div class="${styles.mermaidPlaceholder}"></div></div><div class="${styles.mermaidSource}" data-mermaid-source style="display: none;"><div class="${blockClass}">${code}</div></div></div>\n`;
    }

    return `<div class="${blockClass}"><button type="button" class="${styles.codeCopyBtn}" data-copy-code aria-label="Copy code block" title="Copy code block"><span class="${styles.copyIcon}" aria-hidden="true"></span><span class="${styles.checkIcon}" aria-hidden="true"></span></button>${code}</div>\n`;
  },
};

// CommonMark ends a link destination at the first space, so agents that write
// local paths with spaces emit literal text. Percent-encode those spaces and
// retry the parse; code spans and web URLs stay untouched.
function repairHeadLink(src: string): { source: string; raw: string } | null {
  const head = linkHead.exec(src);
  if (!head) return null;
  const start = head[0].length;
  const end = destinationEnd(src, start);
  if (end < 0) return null;
  const destination = src.slice(start, end);
  const encoded = encodeDestination(destination);
  if (encoded === destination) return null;
  // The repaired destination is longer; the token must consume the original text.
  return { source: src.slice(0, start) + encoded + src.slice(end), raw: src.slice(0, end + 1) };
}

/** Returns the index of the parenthesis that closes a link destination, or -1 across lines. */
function destinationEnd(text: string, open: number): number {
  let depth = 1;
  for (let i = open; i < text.length; i++) {
    const ch = text[i];
    if (ch === "\\") {
      i++;
    } else if (ch === "\n") {
      return -1;
    } else if (ch === "(") {
      depth++;
    } else if (ch === ")" && --depth === 0) {
      return i;
    }
  }
  return -1;
}

const linkHead = /^!?\[[^\]\n]*\]\(/;
const localPathPrefix = /^(?:file:\/\/\/|\/|\.{1,2}\/|~\/)/;
const quotedTitle = /^(.*?)(\s+(?:"[^"]*"|'[^']*'))$/;
const parenTitle = /^([^\s]+)(\s+\([^)]*\))$/;

function encodeDestination(destination: string): string {
  const dest = destination.trim();
  if (dest !== destination || !dest.includes(" ") || !isLocalPath(dest)) return destination;
  const titled = quotedTitle.exec(dest) ?? parenTitle.exec(dest);
  const path = titled?.[1] ?? dest;
  if (!path.includes(" ")) return destination;
  return path.replace(/ /g, "%20") + (titled?.[2] ?? "");
}

/** Accepts absolute, home, and relative paths that name a container file. */
function isLocalPath(dest: string): boolean {
  if (localPathPrefix.test(dest)) return true;
  if (/^[a-z][a-z0-9+.-]*:/i.test(dest) || dest.startsWith("//") || dest.startsWith("#")) return false;
  return dest.includes("/");
}

/**
 * baseDir is the runtime directory of the rendered document. Relative links
 * and images resolve against it; without it the task checkout owns them.
 */
export default function Markdown(props: { text: string; baseDir?: string }) {
  const taskId = useContext(MarkdownTaskContext);
  const navigate = useNavigate();
  const runtimePath = (href: string) => (taskId ? runtimeFilePath(href, props.baseDir ?? null) : null);
  const runtimeTarget = (href: string) => (taskId ? runtimeFileTarget(href, props.baseDir ?? null) : null);
  const marked = new Marked({
    breaks: true,
    gfm: true,
    tokenizer: {
      link(this: Tokenizer, src: string): Tokens.Link | Tokens.Image | undefined {
        const token = Tokenizer.prototype.link.call(this, src);
        if (token) return token;
        const repair = repairHeadLink(src);
        if (!repair) return undefined;
        const repaired = Tokenizer.prototype.link.call(this, repair.source);
        if (repaired) repaired.raw = repair.raw;
        return repaired;
      },
    },
    renderer: {
      ...markdownRenderer,
      link(this: Renderer, token: Tokens.Link): string {
        const target = runtimeTarget(token.href);
        if (target === null || !taskId) return Renderer.prototype.link.call(this, token);
        return Renderer.prototype.link
          .call(this, { ...token, href: taskViewURL(taskId(), target.path, target.line) })
          .replace("<a ", "<a data-app-link ");
      },
      image(this: Renderer, token: Tokens.Image): string {
        const path = runtimePath(token.href);
        if (path === null || !taskId) return Renderer.prototype.image.call(this, token);
        return Renderer.prototype.image.call(this, { ...token, href: taskRawURL(taskId(), path) });
      },
    },
  });
  // Content cannot submit authenticated forms or install page-wide CSS.
  const html = createMemo(() =>
    DOMPurify.sanitize(marked.parse(props.text) as string, {
      USE_PROFILES: { html: true },
      FORBID_TAGS: ["form", "style"],
      FORBID_ATTR: ["form", "formaction", "formenctype", "formmethod", "formtarget"],
    }),
  );
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
    const copiedClass = styles.copied;
    if (text === undefined || text === null) return;

    navigator.clipboard
      .writeText(text)
      .then(() => {
        button.classList.add(copiedClass);
        setTimeout(() => {
          button.classList.remove(copiedClass);
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
          // eslint-disable-next-line solid/no-innerhtml -- DOMPurify sanitizes untrusted Markdown output
          <div class={styles.markdown} innerHTML={html()} />
        }
      >
        <pre class={styles.rawText}>{props.text}</pre>
      </Show>
    </div>
  );
}
