// FileViewerPage mounts one document renderer inside the persistent task workspace.

import { Match, Show, Switch } from "solid-js";
import { useParams, useSearchParams } from "@solidjs/router";

import AttachmentViewer from "../components/AttachmentViewer";
import FileContentViewer from "../components/FileContentViewer";
import CadViewer from "../components/CadViewer";
import { useAppState } from "../AppState";
import { taskIdFromParam, taskPathForTask } from "../taskPath";
import styles from "./FileViewerPage.module.css";
import {
  attachmentPaneMaxDimension,
  inputImageSource,
  isCadPath,
  taskAttachmentLocation,
  taskAttachmentURL,
} from "../runtimePath";

export default function FileViewerPage() {
  const s = useAppState();
  const params = useParams();
  const [query] = useSearchParams();
  const id = () => taskIdFromParam(params.taskId);
  const path = () => (typeof query.path === "string" ? query.path : null);
  const line = () => {
    const value = typeof query.line === "string" ? Number(query.line) : 0;
    return Number.isSafeInteger(value) && value > 0 ? value : null;
  };
  const attachment = () => (typeof query.attachment === "string" ? taskAttachmentLocation(query.attachment) : null);
  const image = () => (typeof query.image === "string" ? inputImageSource(query.image) : null);

  return (
    <Show when={id()} keyed>
      {(taskId) => {
        const t = () => s.taskById(taskId);
        const tp = () => {
          const task = t();
          return task ? taskPathForTask(task) : `/task/@${taskId}`;
        };
        return (
          <Switch>
            <Match when={path()} keyed>
              {(filePath) => (
                <section class={styles.document} aria-label="Document" data-testid="document-pane">
                  <Switch>
                    <Match when={isCadPath(filePath)}>
                      <CadViewer taskId={taskId} taskPath={tp()} path={filePath} />
                    </Match>
                    <Match when={true}>
                      <FileContentViewer taskId={taskId} taskPath={tp()} path={filePath} line={line()} />
                    </Match>
                  </Switch>
                </section>
              )}
            </Match>
            <Match when={attachment()} keyed>
              {(location) => (
                <section class={styles.document} aria-label="Document" data-testid="document-pane">
                  <AttachmentViewer
                    taskPath={tp()}
                    url={taskAttachmentURL(taskId, location, null)}
                    previewURL={taskAttachmentURL(taskId, location, attachmentPaneMaxDimension)}
                  />
                </section>
              )}
            </Match>
            <Match when={image()} keyed>
              {(url) => (
                <section class={styles.document} aria-label="Document" data-testid="document-pane">
                  <AttachmentViewer taskPath={tp()} url={url} previewURL={url} />
                </section>
              )}
            </Match>
          </Switch>
        );
      }}
    </Show>
  );
}
