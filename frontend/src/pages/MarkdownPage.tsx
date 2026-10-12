// MarkdownPage is the /task/:taskId/view route for markdown and CAD files in the task runtime.

import { Match, Show, Switch } from "solid-js";
import { useParams, useSearchParams } from "@solidjs/router";

import MarkdownViewer from "../components/MarkdownViewer";
import CadViewer from "../components/CadViewer";
import { useAppState } from "../AppState";
import { taskIdFromParam, taskPathForTask } from "../taskPath";
import { DetailPane } from "../components/Layout";
import { isCadPath } from "../runtimePath";

export default function MarkdownPage() {
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
                    <MarkdownViewer taskId={taskId} taskPath={tp()} path={filePath} />
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
