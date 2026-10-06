// MarkdownPage is the /task/:taskId/view route for a markdown file in the task runtime.

import { Show } from "solid-js";
import { useParams, useSearchParams } from "@solidjs/router";

import MarkdownViewer from "../components/MarkdownViewer";
import { useAppState } from "../AppState";
import { taskIdFromParam, taskPathForTask } from "../taskPath";
import { DetailPane } from "../components/Layout";

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
              {(filePath) => <MarkdownViewer taskId={taskId} taskPath={tp()} path={filePath} />}
            </Show>
          </DetailPane>
        );
      }}
    </Show>
  );
}
