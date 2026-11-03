// ChartDataTable renders a chart's text alternative, either inline or behind a disclosure.

import { Index } from "solid-js";

import styles from "./ChartDataTable.module.css";

interface DataTableProps {
  regionLabel: string;
  columns: readonly string[];
  rows: readonly (readonly string[])[];
}

export function DataTable(props: DataTableProps) {
  return (
    <div aria-label={props.regionLabel} class={styles.wrap} role="region" tabIndex={0}>
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
  );
}

export default function ChartDataTable(
  props: DataTableProps & {
    caption: string;
    class?: string;
  },
) {
  return (
    <details class={`${styles.details} ${props.class ?? ""}`}>
      <summary>{`${props.caption} (${props.rows.length})`}</summary>
      <DataTable columns={props.columns} regionLabel={props.regionLabel} rows={props.rows} />
    </details>
  );
}
