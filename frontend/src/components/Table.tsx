// Table is the shared scrollable data table: one wrapper, one set of row rules.

import type { JSX } from "solid-js";

import styles from "./Table.module.css";

export default function Table(props: {
  // Names the scroll region for assistive technology.
  label: string;
  // "dense" packs a table that sits in a narrow chart pane.
  dense?: boolean;
  // A page fills its panel; a pane table fits its few columns instead.
  fit?: boolean;
  // A table of labels rather than numbers reads from the left.
  align?: "numbers" | "labels";
  class?: string;
  children: JSX.Element;
}) {
  const table = () =>
    [styles.table, props.dense && styles.dense, props.fit && styles.fit, props.align === "labels" && styles.labels]
      .filter(Boolean)
      .join(" ");
  return (
    <div class={`${styles.wrap} ${props.class ?? ""}`} role="region" aria-label={props.label} tabIndex={0}>
      <table class={table()}>{props.children}</table>
    </div>
  );
}
