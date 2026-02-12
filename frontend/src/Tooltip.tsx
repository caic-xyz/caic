// Tooltip: hover on desktop, tap-to-toggle on mobile.
// Uses a Portal so the popup is never clipped by overflow: hidden ancestors.
import { createSignal, createEffect, Show, onCleanup, type JSX } from "solid-js";
import { Portal } from "solid-js/web";
import styles from "./Tooltip.module.css";

interface Props {
  text: string;
  children: JSX.Element;
}

const GAP = 6; // px between element and popup

export default function Tooltip(props: Props) {
  const [show, setShow] = createSignal(false);
  let wrapperRef: HTMLSpanElement | undefined;
  let popupRef: HTMLSpanElement | undefined;

  function onDocClick(e: MouseEvent) {
    if (wrapperRef && !wrapperRef.contains(e.target as Node)) {
      setShow(false);
    }
  }

  function dismiss() { setShow(false); }

  createEffect(() => {
    if (show()) {
      document.addEventListener("click", onDocClick, true);
      document.addEventListener("scroll", dismiss, true);
    } else {
      document.removeEventListener("click", onDocClick, true);
      document.removeEventListener("scroll", dismiss, true);
    }
  });

  // Position the popup in fixed coordinates relative to the wrapper.
  createEffect(() => {
    if (!show() || !popupRef || !wrapperRef) return;
    const wr = wrapperRef.getBoundingClientRect();
    const pr = popupRef.getBoundingClientRect();
    const margin = 4;

    // Vertical: prefer above, fall back to below.
    if (wr.top - GAP - pr.height < 0) {
      popupRef.style.top = `${wr.bottom + GAP}px`;
    } else {
      popupRef.style.top = `${wr.top - GAP - pr.height}px`;
    }

    // Horizontal: center on wrapper, clamp to viewport.
    let left = wr.left + wr.width / 2 - pr.width / 2;
    left = Math.max(margin, Math.min(left, window.innerWidth - pr.width - margin));
    popupRef.style.left = `${left}px`;
  });

  onCleanup(() => {
    document.removeEventListener("click", onDocClick, true);
    document.removeEventListener("scroll", dismiss, true);
  });

  return (
    <span
      ref={wrapperRef}
      class={styles.wrapper}
      onMouseEnter={() => setShow(true)}
      onMouseLeave={() => setShow(false)}
      onClick={() => setShow((v) => !v)}
    >
      {props.children}
      <Show when={show()}>
        <Portal>
          <span ref={popupRef} class={styles.popup}>{props.text}</span>
        </Portal>
      </Show>
    </span>
  );
}
