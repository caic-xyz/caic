// DiffPage is the /task/:taskId/diff route for a task's file changes.
import { Show } from "solid-js";
import { useParams } from "@solidjs/router";
import DiffDetail from "../components/DiffDetail";
import { useAppState } from "../AppState";
import { taskIdFromParam, taskPath } from "../taskPath";
import styles from "../layout.module.css";

export default function DiffPage() {
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
            <DiffDetail
              taskId={taskId}
              diffStat={t()?.diffStat ?? []}
              repos={(t()?.repos ?? []).map((r) => ({ name: r.name, branch: r.branch }))}
              taskPath={tp()}
            />
          </div>
        );
      }}
    </Show>
  );
}
