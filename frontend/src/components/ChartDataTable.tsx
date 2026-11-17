// ChartDataTable renders a chart's text alternative, inline or behind a disclosure.

import { Index } from "solid-js";

import styles from "./ChartDataTable.module.css";
import Table from "./Table";

interface DataTableProps {
  regionLabel: string;
  columns: readonly string[];
  rows: readonly (readonly string[])[];
  // textColumns lists the indexes of columns holding text, which read from the
  // left; the others hold numbers.
  textColumns?: readonly number[];
  // updatingColumns lists the indexes of columns whose values are refreshing.
  updatingColumns?: readonly number[];
  // fit sizes the table to its columns for a pane narrower than its panel.
  fit?: boolean;
  class?: string;
}

export function DataTable(props: DataTableProps) {
  const align = (i: number) => (props.textColumns?.includes(i) ? "text" : undefined);
  return (
    <Table class={props.class} dense fit={props.fit} label={props.regionLabel}>
      <thead>
        <tr>
          <Index each={props.columns}>
            {(column, i) => (
              <th scope="col" data-align={align(i)}>
                {column()}
              </th>
            )}
          </Index>
        </tr>
      </thead>
      <tbody>
        <Index each={props.rows}>
          {(row) => (
            <tr>
              <Index each={row()}>
                {(cell, i) => (
                  <td
                    data-align={align(i)}
                    class={props.updatingColumns?.includes(i) ? "updating-value" : undefined}
                    aria-busy={props.updatingColumns?.includes(i) ? "true" : undefined}
                  >
                    {cell()}
                  </td>
                )}
              </Index>
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
      <DataTable
        class={styles.collapsed}
        columns={props.columns}
        regionLabel={props.regionLabel}
        rows={props.rows}
        textColumns={props.textColumns}
        updatingColumns={props.updatingColumns}
      />
    </details>
  );
}
