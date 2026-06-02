// ProcessesPage is the /task/:taskId/processes route for a task's process tree.
import { Show } from "solid-js";
import { useParams } from "@solidjs/router";
import ProcessDetail from "../components/ProcessDetail";
import { useAppState } from "../AppState";
import { taskIdFromParam, taskPath } from "../taskPath";
import styles from "../layout.module.css";

export default function ProcessesPage() {
  const s = useAppState();
  const params = useParams();
  const id = () => taskIdFromParam(params.taskId);

  return (
    <Show when={id()} keyed>
      {(taskId) => {
        const t = () => s.taskById(taskId);
        const tp = () => {
          const task = t();
          return task ? taskPath(task.id, task.repos?.[0]?.name ?? "", task.repos?.[0]?.branch ?? "", task.title) : `/task/@${taskId}`;
        };
        return (
          <div class={styles.detailPane}>
            <ProcessDetail
              taskId={taskId}
              repo={t()?.repos?.[0]?.name ?? ""}
              branch={t()?.repos?.[0]?.branch ?? ""}
              taskPath={tp()}
            />
          </div>
        );
      }}
    </Show>
  );
}
