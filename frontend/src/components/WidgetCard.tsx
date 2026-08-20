// Sandboxed iframe widget card for agent-generated HTML widgets.

import { createSignal, createEffect, onCleanup, on, Show } from "solid-js";
import { Portal } from "solid-js/web";

import type { MessageGroup } from "../grouping";
import styles from "./WidgetCard.module.css";

import widgetShellHTML from "./WidgetShell.html?raw";

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
          srcdoc={widgetShellHTML}
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
              srcdoc={widgetShellHTML}
            />
          </div>
        </Portal>
      </Show>
    </>
  );
}
