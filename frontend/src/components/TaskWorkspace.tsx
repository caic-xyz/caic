// Keeps the conversation live beside documents and task subviews across responsive layouts and full screen.
import { type JSX, Show, createSignal, createEffect, onCleanup } from "solid-js";
import { useLocation, useParams } from "@solidjs/router";
import TaskDetailPage from "../pages/TaskDetailPage";
import { taskIdFromParam, taskIdFromPath } from "../taskPath";
import { taskViewMode, rememberTaskViewOrigin, restoreTaskViewOrigin, type TaskViewMode } from "../taskView";
import { TaskTimelineProvider } from "../TaskTimelineContext";
import { useAppState } from "../AppState";
import styles from "./TaskWorkspace.module.css";

export default function TaskWorkspace(props: { children?: JSX.Element }) {
  const location = useLocation();
  const params = useParams();
  const state = useAppState();
  function bindConversation(pane: HTMLDivElement) {
    const remember = (event: MouseEvent) => {
      if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      const link = event.target instanceof Element ? event.target.closest<HTMLAnchorElement>("a") : null;
      if (!link || link.origin !== window.location.origin) return;
      const url = new URL(link.href);
      if (
        taskIdFromPath(url.pathname) === taskIdFromPath(location.pathname) &&
        taskViewMode(url.pathname, url.search) !== "closed"
      ) {
        rememberTaskViewOrigin(link);
      }
    };
    pane.addEventListener("click", remember, true);
    onCleanup(() => pane.removeEventListener("click", remember, true));
  }
  const media = window.matchMedia("(max-width: 768px)");
  const [mobile, setMobile] = createSignal(media.matches);
  const update = () => setMobile(media.matches);
  media.addEventListener("change", update);
  onCleanup(() => media.removeEventListener("change", update));
  const mode = () => taskViewMode(location.pathname, location.search);
  let previousMode: TaskViewMode = "closed";
  let previousPath = location.pathname;
  createEffect(() => {
    const next = mode();
    if (
      next === "closed" &&
      previousMode !== "closed" &&
      /^\/task\/[^/]+$/.test(location.pathname) &&
      taskIdFromPath(location.pathname) === taskIdFromPath(previousPath)
    ) {
      requestAnimationFrame(restoreTaskViewOrigin);
    }
    previousMode = next;
    previousPath = location.pathname;
  });
  const conversationHidden = () => mode() === "fullscreen" || (mobile() && mode() === "docked");
  return (
    <Show when={taskIdFromParam(params.taskId)}>
      {(taskId) => (
        <TaskTimelineProvider
          taskId={taskId()}
          taskState={state.taskById(taskId())?.state ?? "pending"}
          onError={state.showWarning}
        >
          <div
            class={styles.workspace}
            classList={{ [styles.fullscreen]: mode() === "fullscreen" }}
            data-testid="task-workspace"
          >
            <div
              ref={bindConversation}
              class={styles.conversation}
              classList={{ [styles.hidden]: conversationHidden() }}
              inert={conversationHidden()}
              aria-hidden={conversationHidden()}
              data-testid="conversation-pane"
              tabindex={-1}
            >
              <TaskDetailPage />
            </div>
            <div
              class={styles.subview}
              classList={{ [styles.hidden]: mode() === "closed" }}
              data-testid="task-view-pane"
            >
              {props.children}
            </div>
          </div>
        </TaskTimelineProvider>
      )}
    </Show>
  );
}
