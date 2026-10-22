// FileViewerPage mounts one document renderer inside the persistent task workspace.

import { Match, Show, Switch } from "solid-js";
import { useParams, useSearchParams } from "@solidjs/router";

import AttachmentViewer from "../components/AttachmentViewer";
import FileContentViewer from "../components/FileContentViewer";
import CadViewer from "../components/CadViewer";
import { useAppState } from "../AppState";
import { taskIdFromParam, taskPathForTask } from "../taskPath";
import styles from "./FileViewerPage.module.css";
import { isCadPath } from "../runtimePath";

export default function FileViewerPage() {
  const s = useAppState();
  const params = useParams();
  const [query] = useSearchParams();
  const id = () => taskIdFromParam(params.taskId);
  const path = () => (typeof query.path === "string" ? query.path : null);
  const attachment = () => (typeof query.attachment === "string" ? query.attachment : null);

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
                      <FileContentViewer taskId={taskId} taskPath={tp()} path={filePath} />
                    </Match>
                  </Switch>
                </section>
              )}
            </Match>
            <Match when={attachment()} keyed>
              {(hash) => (
                <section class={styles.document} aria-label="Document" data-testid="document-pane">
                  <AttachmentViewer taskId={taskId} taskPath={tp()} hash={hash} />
                </section>
              )}
            </Match>
          </Switch>
        );
      }}
    </Show>
  );
}
