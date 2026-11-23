// TurnTools breaks a turn's or a task's tool time down by tool name: calls, total and, for a turn, longest time, with total-time bars on one scale.

import { For, Show } from "solid-js";

import { formatExactSeconds } from "../timing";
import Table from "./Table";
import styles from "./TurnTools.module.css";

// ToolTime is one tool name's completed calls; a turn also reports its longest call.
export interface ToolTime {
  name: string;
  calls: number;
  durationMs: number;
  longestMs?: number;
}

export default function TurnTools(props: { tools: readonly ToolTime[] }) {
  const longest = () => Math.max(1, ...props.tools.map((tool) => tool.durationMs));
  const showLongest = () => props.tools.every((tool) => tool.longestMs !== undefined);
  return (
    <figure class={styles.tools} data-testid="turn-tools">
      <figcaption class={styles.caption}>
        <span class={styles.title}>Tool time</span>
      </figcaption>
      <Show
        when={props.tools.length > 0}
        fallback={
          <p class={styles.empty} data-testid="turn-tools-empty">
            No tool calls.
          </p>
        }
      >
        <Table label="Tool time" dense>
          <thead>
            <tr>
              <th scope="col">Tool</th>
              <th scope="col">Calls</th>
              <th scope="col">Total</th>
              <Show when={showLongest()}>
                <th scope="col">Longest</th>
              </Show>
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
                  <Show when={showLongest()}>
                    <td>{formatExactSeconds(tool.longestMs ?? 0)}</td>
                  </Show>
                </tr>
              )}
            </For>
          </tbody>
        </Table>
      </Show>
    </figure>
  );
}
