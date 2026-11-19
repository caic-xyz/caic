// Compact status marks for transcript rows: the running spinner and one Material Symbol per outcome.

import { Match, Switch } from "solid-js";
import CheckIcon from "@material-symbols/svg-400/outlined/check.svg?solid";
import ErrorIcon from "@material-symbols/svg-400/outlined/error.svg?solid";
import PauseCircleIcon from "@material-symbols/svg-400/outlined/pause_circle.svg?solid";
import StopCircleIcon from "@material-symbols/svg-400/outlined/stop_circle.svg?solid";

import styles from "./StatusIcon.module.css";

export type StatusIconState = "completed" | "failed" | "interrupted" | "paused" | "running";

const labels: Record<StatusIconState, string> = {
  completed: "Completed",
  failed: "Failed",
  interrupted: "Interrupted",
  paused: "Paused",
  running: "Running",
};

// StatusIcon names its state for assistive technology and on hover. The host
// passes `class` to place the mark within its row.
export default function StatusIcon(props: { state: StatusIconState; class?: string }) {
  return (
    <span
      class={props.class ? `${styles.icon} ${props.class}` : styles.icon}
      data-state={props.state}
      role="img"
      aria-label={labels[props.state]}
      title={labels[props.state]}
    >
      <Switch>
        <Match when={props.state === "running"}>
          <span class={styles.spinner} />
        </Match>
        <Match when={props.state === "completed"}>
          <CheckIcon aria-hidden="true" />
        </Match>
        <Match when={props.state === "failed"}>
          <ErrorIcon aria-hidden="true" />
        </Match>
        <Match when={props.state === "interrupted"}>
          <StopCircleIcon aria-hidden="true" />
        </Match>
        <Match when={props.state === "paused"}>
          <PauseCircleIcon aria-hidden="true" />
        </Match>
      </Switch>
    </span>
  );
}
