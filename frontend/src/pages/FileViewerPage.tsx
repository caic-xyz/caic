// FileViewerPage owns the single /task/:taskId/view route for task-runtime files.

import { Match, Show, Switch } from "solid-js";
import { useParams, useSearchParams } from "@solidjs/router";

import FileContentViewer from "../components/FileContentViewer";
import CadViewer from "../components/CadViewer";
import { useAppState } from "../AppState";
import { taskIdFromParam, taskPathForTask } from "../taskPath";
import { DetailPane } from "../components/Layout";
import { isCadPath } from "../runtimePath";

export default function FileViewerPage() {
  const s = useAppState();
  const params = useParams();
  const [query] = useSearchParams();
  const id = () => taskIdFromParam(params.taskId);
  const path = () => (typeof query.path === "string" ? query.path : null);

  return (
    <Show when={id()} keyed>
      {(taskId) => {
        const t = () => s.taskById(taskId);
        const tp = () => {
          const task = t();
          return task ? taskPathForTask(task) : `/task/@${taskId}`;
        };
        return (
          <DetailPane>
            <Show when={t() ? path() : null} keyed>
              {(filePath) => (
                <Switch>
                  <Match when={isCadPath(filePath)}>
                    <CadViewer taskId={taskId} taskPath={tp()} path={filePath} />
                  </Match>
                  <Match when={true}>
                    <FileContentViewer taskId={taskId} taskPath={tp()} path={filePath} />
                  </Match>
                </Switch>
              )}
            </Show>
          </DetailPane>
        );
      }}
    </Show>
  );
}
