// ChartDataTable is a chart's text alternative: the same numbers as a collapsed table.

import { Index } from "solid-js";

import styles from "./ChartDataTable.module.css";

interface ChartDataTableProps {
  caption: string;
  regionLabel: string;
  columns: readonly string[];
  rows: readonly (readonly string[])[];
  class?: string;
}

export default function ChartDataTable(props: ChartDataTableProps) {
  return (
    <details class={`${styles.details} ${props.class ?? ""}`}>
      <summary>{`${props.caption} (${props.rows.length})`}</summary>
      <div
        aria-label={props.regionLabel}
        class={styles.wrap}
        role="region"
        // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- overflow region must be keyboard-scrollable
        tabIndex={0}
      >
        <table class={styles.table}>
          <thead>
            <tr>
              <Index each={props.columns}>{(column) => <th scope="col">{column()}</th>}</Index>
            </tr>
          </thead>
          <tbody>
            <Index each={props.rows}>
              {(row) => (
                <tr>
                  <Index each={row()}>{(cell) => <td>{cell()}</td>}</Index>
                </tr>
              )}
            </Index>
          </tbody>
        </table>
      </div>
    </details>
  );
}
