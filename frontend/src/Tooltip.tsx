// Tooltip: hover on desktop, tap-to-toggle on mobile.
// Automatically flips below the element when there isn't room above,
// and clamps horizontally to stay within the viewport.
import { createSignal, createEffect, Show, onCleanup, type JSX } from "solid-js";
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

  createEffect(() => {
    if (show()) {
      document.addEventListener("click", onDocClick, true);
    } else {
      document.removeEventListener("click", onDocClick, true);
    }
  });

  // Position the popup after it mounts.
  createEffect(() => {
    if (!show() || !popupRef || !wrapperRef) return;
    const wr = wrapperRef.getBoundingClientRect();
    const pr = popupRef.getBoundingClientRect();

    // Vertical: prefer above, fall back to below.
    if (wr.top - GAP - pr.height < 0) {
      popupRef.style.bottom = "auto";
      popupRef.style.top = `calc(100% + ${GAP}px)`;
    }

    // Horizontal: clamp so the popup stays within the viewport.
    const idealLeft = wr.left + wr.width / 2 - pr.width / 2;
    const margin = 4;
    if (idealLeft < margin) {
      popupRef.style.left = "0";
      popupRef.style.transform = `translateX(${margin - wr.left}px)`;
    } else if (idealLeft + pr.width > window.innerWidth - margin) {
      popupRef.style.left = "auto";
      popupRef.style.right = "0";
      popupRef.style.transform = `translateX(${window.innerWidth - margin - wr.right}px)`;
    }
  });

  onCleanup(() => {
    document.removeEventListener("click", onDocClick, true);
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
        <span ref={popupRef} class={styles.popup}>{props.text}</span>
      </Show>
    </span>
  );
}
