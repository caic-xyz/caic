// Toast notification stack rendered via a portal at the bottom of the viewport.

import { For } from "solid-js";
import { Portal } from "solid-js/web";

import { useAppState } from "../AppState";
import styles from "./Toasts.module.css";

export default function Toasts() {
  const s = useAppState();
  return (
    <Portal>
      <div class={styles.toastContainer}>
        <For each={s.warnings()}>
          {(w) => (
            <div class={styles.toast}>
              <span class={styles.toastMessage}>{w.message}</span>
              <button class={styles.toastDismiss} onClick={() => s.dismissWarning(w.id)}>×</button>
            </div>
          )}
        </For>
      </div>
    </Portal>
  );
}
