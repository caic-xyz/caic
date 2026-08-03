// Modal dialog for cloning a git repository by URL.
// Uses native <dialog> for built-in Escape handling, focus trapping, and backdrop.

import { createSignal, Show } from "solid-js";

import Button from "./Button";
import ModalDialog from "./ModalDialog";
import styles from "./CloneRepoDialog.module.css";

interface Props {
  loading: boolean;
  error: string;
  onClone: (url: string, path?: string) => void;
  onClose: () => void;
}

export default function CloneRepoDialog(props: Props) {
  const [url, setUrl] = createSignal("");
  const [path, setPath] = createSignal("");
  function submit() {
    const u = url().trim();
    if (!u) return;
    const p = path().trim() || undefined;
    props.onClone(u, p);
  }

  return (
    <ModalDialog
      class={styles.dialog}
      onClose={props.onClose}
      dismissOnBackdrop={!props.loading}
      dismissOnEscape={!props.loading}
    >
      <h2 class={styles.title}>Clone Repository</h2>
      <label class={styles.label}>
        URL
        <input
          type="text"
          value={url()}
          onInput={(e) => setUrl(e.currentTarget.value)}
          placeholder="https://github.com/org/repo.git"
          disabled={props.loading}
          class={styles.input}
          data-testid="clone-url"
          autofocus
          onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); submit(); } }}
        />
      </label>
      <label class={styles.label}>
        Path <span class={styles.optional}>(optional)</span>
        <input
          type="text"
          value={path()}
          onInput={(e) => setPath(e.currentTarget.value)}
          placeholder="Derived from URL if empty"
          disabled={props.loading}
          class={styles.input}
          data-testid="clone-path"
          onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); submit(); } }}
        />
      </label>
      <Show when={props.error}><p class={styles.error}>{props.error}</p></Show>
      <div class={styles.actions}>
        <button type="button" class={styles.cancelBtn} onClick={() => props.onClose()} disabled={props.loading}>Cancel</button>
        <Button type="button" onClick={submit} disabled={props.loading || !url().trim()} loading={props.loading} data-testid="clone-submit">Clone</Button>
      </div>
    </ModalDialog>
  );
}
