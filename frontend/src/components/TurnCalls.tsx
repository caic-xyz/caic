// TurnCalls charts each API call's input context against its window and its output, in timeline order, with an exact table.

import { createMemo, For, Show } from "solid-js";

import type { HistoryTurnCall, HistoryTurnTimeline, HistoryTurnTokens } from "@sdk/types.gen";
import { isChildEvent } from "../grouping";
import { formatMoney, formatTokens } from "../formatting";
import { formatExactSeconds } from "../timing";
import ChartDataTable from "./ChartDataTable";
import styles from "./TurnCalls.module.css";

const columns = [
  "Call",
  "Actor",
  "At",
  "New input",
  "Cache write",
  "Cache read",
  "Output",
  "Thinking",
  "Context",
  "Cost",
] as const;
// The actor names a column of text among numbers, so it reads from the left.
const textColumns = [1] as const;

function inputOf(call: HistoryTurnTokens): number {
  return call.inputTokens + call.cacheCreationInputTokens + call.cacheReadInputTokens;
}

function percent(part: number, whole: number): string {
  return `${Math.round((part / whole) * 100)}%`;
}

// labelStep labels every call of a short turn and round call numbers of a
// dense one, so the labels never touch at the narrowest column width.
function labelStep(count: number): number {
  for (const [limit, step] of [
    [12, 1],
    [30, 5],
    [60, 10],
    [150, 25],
    [300, 50],
  ] as const) {
    if (count <= limit) return step;
  }
  return 100;
}

function costText(call: HistoryTurnCall): string {
  if (!call.cost) return "—";
  return `${call.cost.source === "estimated" ? "~" : ""}$${formatMoney(call.cost.usd, 4)}`;
}

// costColumns marks the cost column while estimates refresh.
const costColumns = [columns.indexOf("Cost")] as const;

export default function TurnCalls(props: {
  timeline: HistoryTurnTimeline;
  // costUpdating marks the call costs while their estimates refresh.
  costUpdating: boolean;
}) {
  const calls = () => props.timeline.calls ?? [];
  const actors = createMemo(() => {
    const labels = new Map<string, string>();
    for (const span of props.timeline.spans ?? []) {
      if (span.kind === "subagent" && span.activityID && span.label) labels.set(span.activityID, span.label);
    }
    return labels;
  });
  const actor = (call: HistoryTurnCall) => {
    if (!isChildEvent(call)) return "Agent";
    return (call.activityID && actors().get(call.activityID)) || "Subagent";
  };
  // One explicit domain for every call: the largest of any input context and
  // window, so headroom reads against a common scale.
  const contextMax = createMemo(() =>
    Math.max(1, ...calls().map((call) => Math.max(inputOf(call), call.contextWindow ?? 0))),
  );
  const outputMax = createMemo(() => Math.max(1, ...calls().map((call) => call.outputTokens)));
  const windowed = createMemo(() => calls().some((call) => (call.contextWindow ?? 0) > 0));
  const subagents = createMemo(() => calls().some((call) => isChildEvent(call)));
  // Labels go on the first column, every step-th one, and the last one unless
  // it would crowd the previous label. Positions, not call numbers, set the
  // spacing because the cap can leave gaps in the numbering.
  const labelled = (index: number) => {
    const count = calls().length;
    const step = labelStep(count);
    if (index === 0 || (index + 1) % step === 0) return true;
    return index === count - 1 && count % step >= step / 2;
  };

  const summary = createMemo(() => {
    const parts: string[] = [];
    // Totals cover the omitted calls and the reports no call claims.
    const extra = [props.timeline.omittedCalls, props.timeline.unattributed].filter((tokens) => tokens !== undefined);
    const read = [...calls(), ...extra].reduce((sum, tokens) => sum + tokens.cacheReadInputTokens, 0);
    const input = [...calls(), ...extra].reduce((sum, tokens) => sum + inputOf(tokens), 0);
    if (input > 0) parts.push(`cache read ${percent(read, input)} of input`);
    // Headroom is the root agent's: a subagent has its own context.
    const root = calls().filter((call) => !isChildEvent(call));
    const peak = root.reduce<HistoryTurnCall | null>(
      (best, call) => (!best || inputOf(call) > inputOf(best) ? call : best),
      null,
    );
    if (peak) {
      const window = peak.contextWindow ?? 0;
      const used = inputOf(peak);
      parts.push(
        window > 0
          ? `peak context ${formatTokens(used)} of ${formatTokens(window)} (${percent(used, window)})`
          : `peak context ${formatTokens(used)}, window unknown`,
      );
    }
    const output = [...calls(), ...extra].reduce((sum, tokens) => sum + tokens.outputTokens, 0);
    parts.push(`${formatTokens(output)} output`);
    return parts.join(" · ");
  });

  const rows = createMemo(() =>
    calls().map((call) => [
      String(call.number),
      actor(call),
      formatExactSeconds(call.atMs),
      call.inputTokens.toLocaleString(),
      call.cacheCreationInputTokens.toLocaleString(),
      call.cacheReadInputTokens.toLocaleString(),
      call.outputTokens.toLocaleString(),
      call.reasoningOutputTokens.toLocaleString(),
      call.contextWindow
        ? `${inputOf(call).toLocaleString()} / ${call.contextWindow.toLocaleString()}`
        : inputOf(call).toLocaleString(),
      costText(call),
    ]),
  );

  const omittedText = () => {
    const omitted = props.timeline.omittedCalls;
    if (!omitted || omitted.count === 0) return null;
    const total =
      omitted.inputTokens + omitted.cacheCreationInputTokens + omitted.cacheReadInputTokens + omitted.outputTokens;
    return `Not shown: ${omitted.count} smaller ${omitted.count === 1 ? "call" : "calls"} (${formatTokens(total)} tokens in total).`;
  };
  // Usage reports that cover no single call, such as a cumulative delta after a
  // missed event, count in the turn but in no column.
  const unattributedText = () => {
    const tokens = props.timeline.unattributed;
    if (!tokens) return null;
    const total = inputOf(tokens) + tokens.outputTokens;
    if (total === 0) return null;
    return `Not attributed to a call: ${formatTokens(total)} tokens from usage reports that cover no single call.`;
  };

  return (
    <figure class={styles.calls} data-testid="turn-calls">
      <figcaption class={styles.caption}>
        <span class={styles.title}>Tokens per API call</span>
        <Show when={calls().length > 0}>
          <span class={styles.note}>
            {calls().length} {calls().length === 1 ? "call" : "calls"} in timeline order
          </span>
        </Show>
      </figcaption>
      <Show
        when={calls().length > 0}
        fallback={
          <p class={styles.empty} data-testid="turn-calls-empty">
            This harness reports token usage per turn, not per API call; the turn totals are in the details below.
          </p>
        }
      >
        <p class={styles.summary} data-testid="turn-calls-summary">
          {summary()}
        </p>
        <ul class={styles.legend} aria-hidden="true">
          <li>
            <span class={`${styles.swatch} ${styles.cacheRead}`} />
            Cache read
          </li>
          <li>
            <span class={`${styles.swatch} ${styles.cacheWrite}`} />
            Cache write
          </li>
          <li>
            <span class={`${styles.swatch} ${styles.newInput}`} />
            New input
          </li>
          <Show when={windowed()}>
            <li>
              <span class={`${styles.swatch} ${styles.windowSwatch}`} />
              Context window
            </li>
          </Show>
          <li>
            <span class={`${styles.swatch} ${styles.output}`} />
            Output
          </li>
          <li>
            <span class={`${styles.swatch} ${styles.reasoning}`} />
            Thinking
          </li>
          <Show when={subagents()}>
            <li>
              <span class={`${styles.swatch} ${styles.subagentSwatch}`} />
              Subagent call
            </li>
          </Show>
        </ul>
        <div
          class={styles.chart}
          role="img"
          aria-label={`Tokens per API call, ${calls().length} calls: ${summary()}. The calls table lists exact values.`}
          data-testid="turn-calls-chart"
        >
          <div class={styles.scales} aria-hidden="true">
            <div class={styles.contextScale}>
              <span>{formatTokens(contextMax())}</span>
              <span class={styles.scaleName}>input</span>
              <span>0</span>
            </div>
            <div class={styles.outputScale}>
              <span>{formatTokens(outputMax())}</span>
              <span class={styles.scaleName}>output</span>
              <span>0</span>
            </div>
          </div>
          <ol class={styles.columns}>
            <For each={calls()}>
              {(call, index) => {
                const share = (value: number) => value / contextMax();
                return (
                  <li
                    class={styles.column}
                    classList={{ [styles.child]: isChildEvent(call) }}
                    title={`Call ${call.number} · ${actor(call)} · at ${formatExactSeconds(call.atMs)} · input ${inputOf(call).toLocaleString()} · output ${call.outputTokens.toLocaleString()}`}
                    data-testid="turn-call"
                  >
                    <div class={styles.context}>
                      <span
                        class={`${styles.segment} ${styles.cacheRead}`}
                        style={{ "--share": share(call.cacheReadInputTokens) }}
                      />
                      <span
                        class={`${styles.segment} ${styles.cacheWrite}`}
                        style={{ "--share": share(call.cacheCreationInputTokens) }}
                      />
                      <span
                        class={`${styles.segment} ${styles.newInput}`}
                        style={{ "--share": share(call.inputTokens) }}
                      />
                      <Show when={(call.contextWindow ?? 0) > 0}>
                        <span class={styles.window} style={{ "--share": share(call.contextWindow ?? 0) }} />
                      </Show>
                    </div>
                    <div class={styles.outputPanel}>
                      <span
                        class={`${styles.segment} ${styles.reasoning}`}
                        style={{ "--share": call.reasoningOutputTokens / outputMax() }}
                      />
                      <span
                        class={`${styles.segment} ${styles.output}`}
                        style={{ "--share": (call.outputTokens - call.reasoningOutputTokens) / outputMax() }}
                      />
                    </div>
                    <span class={styles.label}>{labelled(index()) ? call.number : ""}</span>
                  </li>
                );
              }}
            </For>
          </ol>
        </div>
        <Show when={omittedText()}>
          {(text) => (
            <p class={styles.note} data-testid="turn-calls-omitted">
              {text()}
            </p>
          )}
        </Show>
        <Show when={unattributedText()}>
          {(text) => (
            <p class={styles.note} data-testid="turn-calls-unattributed">
              {text()}
            </p>
          )}
        </Show>
        <ChartDataTable
          caption="Call tokens"
          regionLabel="Call tokens"
          columns={columns}
          textColumns={textColumns}
          updatingColumns={props.costUpdating ? costColumns : undefined}
          rows={rows()}
        />
      </Show>
    </figure>
  );
}
