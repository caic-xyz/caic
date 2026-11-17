// Duration renders an elapsed time in the transcript's shared duration style; callers own its placement.

import type { JSX } from "solid-js";

import styles from "./Duration.module.css";

// Duration reads each prop in place. splitProps would copy the caller's getters
// onto new objects, and V8 can keep such a getter in a shared hidden class after
// the copy dies: its closure then pins the caller's detached DOM and data.
export default function Duration(props: { class?: string; "data-testid"?: string; children: JSX.Element }) {
  return (
    <span
      class={props.class ? `${styles.duration} ${props.class}` : styles.duration}
      data-testid={props["data-testid"]}
    >
      {props.children}
    </span>
  );
}
