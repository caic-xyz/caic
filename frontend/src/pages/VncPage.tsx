// VncPage is the /task/:taskId/vnc route with the lazily-loaded noVNC desktop viewer.

import { Show, Suspense, lazy } from "solid-js";
import { useParams } from "@solidjs/router";

import { useAppState } from "../AppState";
import { taskIdFromParam, taskPath } from "../taskPath";
import { DetailPane } from "../components/Layout";

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
          return task ? taskPath(task.id, task.repos?.[0]?.name ?? "", task.repos?.[0]?.branch ?? "", task.title) : `/task/@${taskId}`;
        };
        return (
          <DetailPane>
            <Suspense fallback={<div style={{ padding: "1rem", color: "var(--color-text-muted)" }}>Loading VNC viewer…</div>}>
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
