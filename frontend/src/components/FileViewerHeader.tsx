// Document header adds a path breadcrumb, raw access, and renderer actions to shared task-view controls.
import { For, Show, createMemo, type JSX } from "solid-js";
import { A } from "@solidjs/router";
import { taskRawURL, taskViewURL } from "../runtimePath";
import TaskViewHeader from "./TaskViewHeader";
import styles from "./FileViewerHeader.module.css";

interface Crumb {
  name: string;
  path: string;
}

// pathCrumbs splits an absolute runtime path into linkable prefixes.
function pathCrumbs(path: string): Crumb[] | null {
  if (!path.startsWith("/")) return null;
  const crumbs: Crumb[] = [{ name: "/", path: "/" }];
  let current = "";
  for (const part of path.split("/")) {
    if (part === "") continue;
    current += `/${part}`;
    crumbs.push({ name: part, path: current });
  }
  return crumbs;
}

export default function FileViewerHeader(props: {
  taskId: string;
  taskPath: string;
  path: string;
  directory?: boolean;
  children?: JSX.Element;
}) {
  const crumbs = createMemo(() => pathCrumbs(props.path));
  return (
    <TaskViewHeader
      taskPath={props.taskPath}
      label="Document controls"
      actions={
        <>
          <Show when={!props.directory}>
            <a
              class={styles.rawLink}
              href={taskRawURL(props.taskId, props.path)}
              target="_blank"
              rel="noopener noreferrer"
            >
              raw file
            </a>
          </Show>
          {props.children}
        </>
      }
    >
      <Show
        when={crumbs()}
        fallback={
          <span class={styles.path} title={props.path}>
            {props.path}
          </span>
        }
      >
        {(parts) => (
          <nav class={styles.path} aria-label="Path" title={props.path}>
            <For each={parts()}>
              {(crumb, index) => (
                <>
                  <Show when={index() > 1}>
                    <span aria-hidden="true">/</span>
                  </Show>
                  <Show
                    when={index() < parts().length - 1}
                    fallback={
                      <span class={styles.crumbCurrent} aria-current="page">
                        {crumb.name}
                      </span>
                    }
                  >
                    <A class={styles.crumb} href={taskViewURL(props.taskId, crumb.path)}>
                      {crumb.name}
                    </A>
                  </Show>
                </>
              )}
            </For>
          </nav>
        )}
      </Show>
    </TaskViewHeader>
  );
}
