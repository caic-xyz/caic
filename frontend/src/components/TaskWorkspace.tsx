// Keeps live task and document panes mounted across responsive selection and full screen.
import { type JSX, createSignal, createEffect, onCleanup } from "solid-js";
import { useLocation } from "@solidjs/router";
import TaskDetailPage from "../pages/TaskDetailPage";
import { taskIdFromPath } from "../taskPath";
import { fileViewMode, restoreFileOrigin, type FileViewMode } from "../fileView";
import styles from "./TaskWorkspace.module.css";

export default function TaskWorkspace(props: { children?: JSX.Element }) {
  const location = useLocation();
  const media = window.matchMedia("(max-width: 768px)");
  const [mobile, setMobile] = createSignal(media.matches);
  const update = () => setMobile(media.matches);
  media.addEventListener("change", update);
  onCleanup(() => media.removeEventListener("change", update));
  const mode = () => fileViewMode(location.pathname, location.search);
  let previousMode: FileViewMode = "closed";
  let previousPath = location.pathname;
  createEffect(() => {
    const next = mode();
    if (
      next === "closed" &&
      previousMode !== "closed" &&
      /^\/task\/[^/]+$/.test(location.pathname) &&
      taskIdFromPath(location.pathname) === taskIdFromPath(previousPath)
    ) {
      requestAnimationFrame(restoreFileOrigin);
    }
    previousMode = next;
    previousPath = location.pathname;
  });
  const conversationHidden = () => mode() === "fullscreen" || (mobile() && mode() === "docked");
  return (
    <div
      class={styles.workspace}
      classList={{ [styles.fullscreen]: mode() === "fullscreen" }}
      data-testid="task-workspace"
    >
      <div
        class={styles.conversation}
        classList={{ [styles.hidden]: conversationHidden() }}
        inert={conversationHidden()}
        aria-hidden={conversationHidden()}
        data-testid="conversation-pane"
        tabindex={-1}
      >
        <TaskDetailPage />
      </div>
      <div class={styles.document} classList={{ [styles.hidden]: mode() === "closed" }}>
        {props.children}
      </div>
    </div>
  );
}
