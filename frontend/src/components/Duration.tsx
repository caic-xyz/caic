// Duration renders an elapsed time in the transcript's shared duration style; callers own its placement.

import { splitProps, type JSX } from "solid-js";

import styles from "./Duration.module.css";

export default function Duration(props: JSX.HTMLAttributes<HTMLSpanElement>) {
  const [local, rest] = splitProps(props, ["class"]);
  return <span {...rest} class={local.class ? `${styles.duration} ${local.class}` : styles.duration} />;
}
