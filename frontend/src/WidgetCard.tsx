// Sandboxed iframe widget card for agent-generated HTML widgets.
import { createSignal, createEffect, onCleanup, on, Show } from "solid-js";
import { Portal } from "solid-js/web";
import type { MessageGroup } from "./grouping";
import styles from "./WidgetCard.module.css";

// Shell HTML loaded into the iframe via srcdoc. Includes morphdom for DOM
// diffing, a _setContent function for streaming updates, _runScripts for
// activating <script> tags on completion, and a ResizeObserver that posts
// height changes to the parent.
const SHELL_HTML = [
  "<!DOCTYPE html>",
  '<html data-theme="light"><head>',
  '<meta charset="utf-8">',
  '<meta http-equiv="Content-Security-Policy"',
  '      content="default-src \'none\'; script-src \'unsafe-inline\' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com https://esm.sh; style-src \'unsafe-inline\'; img-src https: data:; font-src https: data:;">',
  '<script src="https://cdn.jsdelivr.net/npm/morphdom@2/dist/morphdom-umd.min.js"></script>',
  "<style>",
  ":root { color-scheme: light dark; }",
  "body { margin: 0; padding: 8px; background: #fff; color: #1a1a1a; font-family: system-ui, sans-serif; }",
  "._fadeIn { animation: _fadeIn 0.3s ease; }",
  "@keyframes _fadeIn { from { opacity: 0 } to { opacity: 1 } }",
  "</style>",
  "</head><body>",
  '<div id="root"></div>',
  "<script>",
  "window.onerror = function(msg, url, line) {",
  "  parent.postMessage({ type: 'widgetError', message: msg + ' (line ' + line + ')' }, '*');",
  "};",
  "window._setContent = function(html) {",
  "  var root = document.getElementById('root');",
  "  var tmp = document.createElement('div');",
  "  tmp.innerHTML = html;",
  "  if (typeof morphdom !== 'undefined') {",
  "    morphdom(root, tmp, {",
  "      childrenOnly: true,",
  "      onBeforeElUpdated: function(from, to) { return !from.isEqualNode(to); },",
  "      onNodeAdded: function(node) { if (node.classList) node.classList.add('_fadeIn'); return node; }",
  "    });",
  "  } else {",
  "    root.innerHTML = html;",
  "  }",
  "};",
  "window._runScripts = function() {",
  "  document.querySelectorAll('#root script').forEach(function(old) {",
  "    var s = document.createElement('script');",
  "    if (old.src) s.src = old.src;",
  "    else s.textContent = old.textContent;",
  "    old.parentNode.replaceChild(s, old);",
  "  });",
  "};",
  "window.addEventListener('message', function(e) {",
  "  if (e.data && e.data.type === 'setContent') window._setContent(e.data.html);",
  "  if (e.data && e.data.type === 'runScripts') window._runScripts();",
  "  if (e.data && e.data.type === 'setTheme')",
  "    document.documentElement.setAttribute('data-theme', e.data.theme);",
  "});",
  "new ResizeObserver(function() {",
  "  parent.postMessage({ type: 'resize', height: document.body.scrollHeight }, '*');",
  "}).observe(document.body);",
  "parent.postMessage({ type: 'ready' }, '*');",
  "</script>",
  "</body></html>",
].join("\n");

export default function WidgetCard(props: { group: MessageGroup }) {
  const [iframeHeight, setIframeHeight] = createSignal(400);
  const [iframeReady, setIframeReady] = createSignal(false);
  const [fullscreen, setFullscreen] = createSignal(false);
  let iframeRef: HTMLIFrameElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref
  let fullscreenIframeRef: HTMLIFrameElement | undefined; // eslint-disable-line no-unassigned-vars -- assigned by SolidJS ref
  const [fullscreenReady, setFullscreenReady] = createSignal(false);

  function postContent(html: string, final: boolean) {
    if (!iframeRef?.contentWindow) return;
    iframeRef.contentWindow.postMessage({ type: "setContent", html }, "*");
    if (final) {
      iframeRef.contentWindow.postMessage({ type: "runScripts" }, "*");
    }
  }

  function postFullscreenContent(html: string, final: boolean) {
    if (!fullscreenIframeRef?.contentWindow) return;
    fullscreenIframeRef.contentWindow.postMessage({ type: "setContent", html }, "*");
    if (final) {
      fullscreenIframeRef.contentWindow.postMessage({ type: "runScripts" }, "*");
    }
  }

  // Track last posted HTML to avoid redundant messages.
  let lastPostedHTML = "";
  let lastPostedFullscreenHTML = "";

  // Listen for messages from the iframes.
  function onMessage(e: MessageEvent) {
    if (e.source === iframeRef?.contentWindow) {
      if (e.data?.type === "resize") {
        const h = Math.max(100, Math.min(2000, e.data.height + 20));
        setIframeHeight(h);
      } else if (e.data?.type === "ready") {
        setIframeReady(true);
      }
    } else if (e.source === fullscreenIframeRef?.contentWindow) {
      if (e.data?.type === "ready") {
        setFullscreenReady(true);
      }
    }
  }

  // Post content when HTML changes, widget completes, or iframe becomes ready.
  createEffect(on(
    () => [props.group.widgetHTML, props.group.widgetDone, iframeReady()] as const,
    ([html, done, ready]) => {
      if (!html || !ready) return;
      const final = !!done;
      if (html !== lastPostedHTML) {
        lastPostedHTML = html;
        postContent(html, final);
      } else if (final) {
        // HTML unchanged but just became done — run scripts.
        iframeRef?.contentWindow?.postMessage({ type: "runScripts" }, "*");
      }
    },
  ));

  // Mirror content to fullscreen iframe when it's open and ready.
  createEffect(on(
    () => [props.group.widgetHTML, props.group.widgetDone, fullscreenReady(), fullscreen()] as const,
    ([html, done, ready, fs]) => {
      if (!html || !ready || !fs) return;
      const final = !!done;
      if (html !== lastPostedFullscreenHTML) {
        lastPostedFullscreenHTML = html;
        postFullscreenContent(html, final);
      } else if (final) {
        fullscreenIframeRef?.contentWindow?.postMessage({ type: "runScripts" }, "*");
      }
    },
  ));

  // Close fullscreen on Escape.
  function onKeyDown(e: KeyboardEvent) {
    if (e.key === "Escape" && fullscreen()) {
      setFullscreen(false);
    }
  }

  // Reset fullscreen iframe state when closing.
  createEffect(on(
    () => fullscreen(),
    (fs) => {
      if (!fs) {
        setFullscreenReady(false);
        lastPostedFullscreenHTML = "";
      }
    },
  ));

  window.addEventListener("message", onMessage);
  window.addEventListener("keydown", onKeyDown);
  onCleanup(() => {
    window.removeEventListener("message", onMessage);
    window.removeEventListener("keydown", onKeyDown);
  });

  return (
    <>
      <div class={styles.widgetCard}>
        <div class={styles.widgetHeader}>
          <span class={styles.widgetTitle}>{props.group.widgetTitle || "Widget"}</span>
          <span class={styles.widgetBadge}>{props.group.widgetDone ? "\u2713" : "\u25CF streaming"}</span>
          <button class={styles.fullscreenBtn} onClick={() => setFullscreen(true)} title="Fullscreen" aria-label="Fullscreen">
            {"\u26F6"}
          </button>
        </div>
        <iframe
          ref={iframeRef}
          title={props.group.widgetTitle || "Widget"}
          class={styles.widgetIframe}
          sandbox="allow-scripts"
          srcdoc={SHELL_HTML}
          style={{ height: `${iframeHeight()}px` }}
        />
      </div>
      <Show when={fullscreen()}>
        <Portal>
          {/* eslint-disable-next-line jsx-a11y/no-static-element-interactions, jsx-a11y/click-events-have-key-events -- backdrop dismiss is supplementary to close button and Escape key */}
          <div class={styles.fullscreenOverlay} onClick={(e) => { if (e.target === e.currentTarget) setFullscreen(false); }}>
            <div class={styles.fullscreenHeader}>
              <span class={styles.widgetTitle}>{props.group.widgetTitle || "Widget"}</span>
              <span class={styles.widgetBadge}>{props.group.widgetDone ? "\u2713" : "\u25CF streaming"}</span>
              <button class={styles.fullscreenCloseBtn} onClick={() => setFullscreen(false)} title="Close fullscreen" aria-label="Close fullscreen">
                {"\u2715"}
              </button>
            </div>
            <iframe
              ref={fullscreenIframeRef}
              title={props.group.widgetTitle || "Widget"}
              class={styles.fullscreenIframe}
              sandbox="allow-scripts"
              srcdoc={SHELL_HTML}
            />
          </div>
        </Portal>
      </Show>
    </>
  );
}
