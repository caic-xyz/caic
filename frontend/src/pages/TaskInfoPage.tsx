// TaskInfoPage is the /task/:taskId/info route for recorded and observed task metadata.

import { Show } from "solid-js";
import { useParams } from "@solidjs/router";

import TaskInfo from "../components/TaskInfo";
import { useAppState } from "../AppState";
import { taskIdFromParam, taskPathForTask } from "../taskPath";
import { DetailPane } from "../components/Layout";

export default function TaskInfoPage() {
  const s = useAppState();
  const params = useParams();
  const id = () => taskIdFromParam(params.taskId);

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
            <TaskInfo
              taskId={taskId}
              repo={t()?.repos?.[0]?.name ?? ""}
              branch={t()?.repos?.[0]?.branch ?? ""}
              taskPath={tp()}
              onTaskRefreshError={s.dismissSelectedTaskOnNotFound}
            />
          </DetailPane>
        );
      }}
    </Show>
  );
}
