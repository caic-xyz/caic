// Fits document images to their pane and toggles scrollable actual-size viewing.

import { createSignal } from "solid-js";
import styles from "./ImagePreview.module.css";

export default function ImagePreview(props: { src: string; fullSrc: string; alt: string; onError: () => void }) {
  const [zoomed, setZoomed] = createSignal(false);
  const action = () => (zoomed() ? "Fit image to pane" : "Zoom image to actual size");
  return (
    <div class={styles.viewport}>
      <button
        type="button"
        class={styles.imageButton}
        classList={{ [styles.zoomed]: zoomed() }}
        aria-label={action()}
        title={action()}
        onClick={() => setZoomed((value) => !value)}
      >
        <img
          class={styles.image}
          src={zoomed() ? props.fullSrc : props.src}
          alt={props.alt}
          onError={() => props.onError()}
        />
      </button>
    </div>
  );
}
