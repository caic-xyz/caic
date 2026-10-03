// Toast notification stack with categorized warning details rendered in a viewport portal.

import { For, Show } from "solid-js";
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
              <div class={styles.toastMessage}>
                <span>{w.message}</span>
                <Show when={w.details.length > 0}>
                  <details>
                    <summary>Details</summary>
                    <ul>
                      <For each={w.details}>
                        {(d) => (
                          <li>
                            <strong>{d.repo}</strong>: {d.error}
                          </li>
                        )}
                      </For>
                    </ul>
                  </details>
                </Show>
              </div>
              <button class={styles.toastDismiss} aria-label="Dismiss warning" onClick={() => s.dismissWarning(w.id)}>
                ×
              </button>
            </div>
          )}
        </For>
      </div>
    </Portal>
  );
}
