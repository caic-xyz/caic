// ChartDataTable renders a chart's text alternative, inline or behind a disclosure.

import { Index } from "solid-js";

import styles from "./ChartDataTable.module.css";
import Table from "./Table";

interface DataTableProps {
  regionLabel: string;
  columns: readonly string[];
  rows: readonly (readonly string[])[];
  class?: string;
}

export function DataTable(props: DataTableProps) {
  return (
    <Table class={props.class} dense label={props.regionLabel}>
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
    </Table>
  );
}

export default function ChartDataTable(
  props: DataTableProps & {
    caption: string;
  },
) {
  return (
    <details class={`${styles.details} ${props.class ?? ""}`}>
      <summary>{`${props.caption} (${props.rows.length})`}</summary>
      <DataTable class={styles.collapsed} columns={props.columns} regionLabel={props.regionLabel} rows={props.rows} />
    </details>
  );
}
