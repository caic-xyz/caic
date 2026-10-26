// Renders one prompt image from the task log inside the document pane.

import { Show, createSignal } from "solid-js";

import ImagePreview from "./ImagePreview";
import TaskViewHeader from "./TaskViewHeader";
import styles from "./AttachmentViewer.module.css";
import { attachmentPaneWidth, taskAttachmentURL } from "../runtimePath";

export default function AttachmentViewer(props: { taskId: string; taskPath: string; hash: string }) {
  const [failed, setFailed] = createSignal(false);
  const full = () => taskAttachmentURL(props.taskId, props.hash, null);
  const pane = () => taskAttachmentURL(props.taskId, props.hash, attachmentPaneWidth);
  return (
    <div class={styles.container}>
      <TaskViewHeader
        taskPath={props.taskPath}
        label="Attachment controls"
        actions={
          <a class={styles.rawLink} href={full()} target="_blank" rel="noopener noreferrer">
            full image
          </a>
        }
      >
        <span class={styles.title} title={props.hash}>
          Attached image
        </span>
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
          <ImagePreview src={pane()} fullSrc={full()} alt="attachment" onError={() => setFailed(true)} />
        </Show>
      </div>
    </div>
  );
}
