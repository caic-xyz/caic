// TurnTools breaks a turn's tool time down by tool name: calls, total and longest time, with total-time bars on one scale.

import { For, Show } from "solid-js";

import type { HistoryTurnTool } from "@sdk/types.gen";
import { formatExactSeconds } from "../timing";
import Table from "./Table";
import styles from "./TurnTools.module.css";

export default function TurnTools(props: { tools: readonly HistoryTurnTool[] }) {
  const longest = () => Math.max(1, ...props.tools.map((tool) => tool.durationMs));
  return (
    <figure class={styles.tools} data-testid="turn-tools">
      <figcaption class={styles.caption}>
        <span class={styles.title}>Tool time</span>
        <Show when={props.tools.length > 0}>
          <span class={styles.note}>Concurrent calls overlap, so totals can exceed the turn time.</span>
        </Show>
      </figcaption>
      <Show
        when={props.tools.length > 0}
        fallback={
          <p class={styles.empty} data-testid="turn-tools-empty">
            No tool calls in this turn.
          </p>
        }
      >
        <Table label="Tool time" dense>
          <thead>
            <tr>
              <th scope="col">Tool</th>
              <th scope="col">Calls</th>
              <th scope="col">Total</th>
              <th scope="col">Longest</th>
            </tr>
          </thead>
          <tbody>
            <For each={props.tools}>
              {(tool) => (
                <tr data-testid="turn-tool">
                  <th scope="row" class={styles.name}>
                    {tool.name}
                    <span class={styles.bar} style={{ "--share": tool.durationMs / longest() }} />
                  </th>
                  <td>{tool.calls.toLocaleString()}</td>
                  <td>{formatExactSeconds(tool.durationMs)}</td>
                  <td>{formatExactSeconds(tool.longestMs)}</td>
                </tr>
              )}
            </For>
          </tbody>
        </Table>
      </Show>
    </figure>
  );
}
