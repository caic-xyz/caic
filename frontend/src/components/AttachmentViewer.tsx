// Renders immutable prompt image resources inside the document pane.

import { Show, createSignal } from "solid-js";

import ImagePreview from "./ImagePreview";
import TaskViewHeader from "./TaskViewHeader";
import styles from "./AttachmentViewer.module.css";

export default function AttachmentViewer(props: {
  taskPath: string;
  url: string;
  previewURL: string;
  scrollWithPage?: boolean;
}) {
  const [failed, setFailed] = createSignal(false);
  return (
    <div class={`${styles.container} ${props.scrollWithPage ? styles.scrolling : ""}`}>
      <TaskViewHeader
        taskPath={props.taskPath}
        label="Attachment controls"
        actions={
          <a class={styles.rawLink} href={props.url} target="_blank" rel="noopener noreferrer">
            full image
          </a>
        }
      >
        <span class={styles.title}>Attached image</span>
      </TaskViewHeader>
      <div class={styles.content}>
        <Show
          when={!failed()}
          fallback={
            <div class={styles.error} role="alert">
              Image unavailable
            </div>
          }
        >
          <ImagePreview src={props.previewURL} fullSrc={props.url} alt="attachment" onError={() => setFailed(true)} />
        </Show>
      </div>
    </div>
  );
}
