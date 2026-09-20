// VncPage is the /task/:taskId/vnc route with the lazily-loaded noVNC desktop viewer.

import { Show, Suspense, lazy } from "solid-js";
import { useParams } from "@solidjs/router";

import { useAppState } from "../AppState";
import { taskIdFromParam, taskPathForTask } from "../taskPath";
import { DetailPane } from "../components/Layout";
import styles from "./VncPage.module.css";

const VncViewer = lazy(() => import("../components/VncViewer"));

export default function VncPage() {
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
            <Suspense fallback={<div class={styles.loading}>Loading VNC viewer…</div>}>
              <VncViewer
                taskId={taskId}
                repo={t()?.repos?.[0]?.name ?? ""}
                branch={t()?.repos?.[0]?.branch ?? ""}
                taskPath={tp()}
              />
            </Suspense>
          </DetailPane>
        );
      }}
    </Show>
  );
}
