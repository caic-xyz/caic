// Document header adds file identity, raw access, and renderer actions to shared task-view controls.
import { type JSX } from "solid-js";
import { taskRawURL } from "../runtimePath";
import TaskViewHeader from "./TaskViewHeader";
import styles from "./FileViewerHeader.module.css";

export default function FileViewerHeader(props: {
  taskId: string;
  taskPath: string;
  path: string;
  children?: JSX.Element;
}) {
  return (
    <TaskViewHeader
      taskPath={props.taskPath}
      label="Document controls"
      actions={
        <>
          <a
            class={styles.rawLink}
            href={taskRawURL(props.taskId, props.path)}
            target="_blank"
            rel="noopener noreferrer"
          >
            raw file
          </a>
          {props.children}
        </>
      }
    >
      <span class={styles.path} title={props.path}>
        {props.path}
      </span>
    </TaskViewHeader>
  );
}
