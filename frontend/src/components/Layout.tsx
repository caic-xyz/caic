// Shared route layout wrappers for split panes and detail-pane slots.
import type { JSX } from "solid-js";
import styles from "./Layout.module.css";

export function Layout(props: { children?: JSX.Element }) {
  return <div class={styles.layout}>{props.children}</div>;
}

export function DetailPane(props: { children?: JSX.Element }) {
  return <div class={styles.detailPane}>{props.children}</div>;
}
