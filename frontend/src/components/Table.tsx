// Table is the shared scrollable data table: one wrapper, one set of row rules.

import type { JSX } from "solid-js";

import styles from "./Table.module.css";

export default function Table(props: {
  // Names the scroll region for assistive technology.
  label: string;
  // "dense" packs a table that sits in a narrow chart pane.
  dense?: boolean;
  class?: string;
  children: JSX.Element;
}) {
  return (
    <div class={`${styles.wrap} ${props.class ?? ""}`} role="region" aria-label={props.label} tabIndex={0}>
      <table class={props.dense ? `${styles.table} ${styles.dense}` : styles.table}>{props.children}</table>
    </div>
  );
}
