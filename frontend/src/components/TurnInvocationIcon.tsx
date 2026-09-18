// TurnInvocationIcon opens per-turn and aggregate session invocation and change details.

import { createSignal, Show } from "solid-js";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import InfoIcon from "@material-symbols/svg-400/outlined/info.svg?solid";

import type { EventChangeStat } from "@sdk/types.gen";
import type { TurnTiming } from "../timing";
import { formatTimingDuration } from "../timing";
import ModalDialog from "./ModalDialog";
import styles from "./TurnInvocationIcon.module.css";

function formatTokens(tokens: number): string {
  if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(1)}Mt`;
  if (tokens >= 1_000) return `${(tokens / 1_000).toFixed(1)}kt`;
  return `${tokens}t`;
}

function formatUSD(usd: number): string {
  return `$${usd.toFixed(usd < 0.01 ? 4 : 2)}`;
}

function formatTotalDuration(ms: number): string {
  return ms > 0 ? formatTimingDuration(ms) : "0s";
}

function formatChangeStat(stat: EventChangeStat, aggregate: boolean): string {
  const files = aggregate
    ? `${stat.files} ${stat.files === 1 ? "file change" : "file changes"}`
    : `${stat.files} ${stat.files === 1 ? "file" : "files"}`;
  const binary =
    stat.binaryFiles > 0
      ? ` · ${stat.binaryFiles} ${stat.binaryFiles === 1 ? "binary" : "binaries"}`
      : "";
  return `${files} · +${stat.added} −${stat.deleted}${binary}`;
}

export default function TurnInvocationIcon(props: { turn: TurnTiming; model: string | null }) {
  const [open, setOpen] = createSignal(false);
  const result = () => props.turn.result;
  const usage = () => result().usage;
  const model = () => usage().reportedModel || props.turn.reportedModel || props.model;

  return (
    <>
      <button
        type="button"
        class={styles.trigger}
        onClick={() => setOpen(true)}
        aria-label="Turn invocation details"
        title="Turn invocation details"
        data-testid="turn-invocation-trigger"
      >
        <InfoIcon width="13" height="13" aria-hidden="true" />
      </button>
      <Show when={open()}>
        <ModalDialog
          class={styles.dialog}
          onClose={() => setOpen(false)}
          data-testid="turn-invocation-dialog"
        >
          <div class={styles.heading}>
            <h2>Turn details</h2>
            <button
              type="button"
              class={styles.close}
              onClick={() => setOpen(false)}
              aria-label="Close turn details"
              title="Close turn details"
            >
              <CloseIcon width="18" height="18" aria-hidden="true" />
            </button>
          </div>
          <dl class={styles.metrics}>
            <Show when={model()} keyed>
              {(name) => (
                <div>
                  <dt>Model</dt>
                  <dd>{name}</dd>
                </div>
              )}
            </Show>
            <div>
              <dt>Turn time</dt>
              <dd>{formatTimingDuration(result().duration * 1_000)}</dd>
            </div>
            <Show when={result().durationAPI > 0}>
              <div>
                <dt>API time</dt>
                <dd>{formatTimingDuration(result().durationAPI * 1_000)}</dd>
              </div>
            </Show>
            <Show when={props.turn.waitMs !== null && props.turn.waitMs > 0}>
              <div>
                <dt>User wait</dt>
                <dd>{formatTimingDuration(props.turn.waitMs ?? 0)}</dd>
              </div>
            </Show>
            <Show when={props.turn.changeStat} keyed>
              {(stat) => (
                <div>
                  <dt>Generated change</dt>
                  <dd>{formatChangeStat(stat, false)}</dd>
                </div>
              )}
            </Show>
            <Show when={result().totalCostUSD > 0}>
              <div>
                <dt>Cost</dt>
                <dd>{formatUSD(result().totalCostUSD)}</dd>
              </div>
            </Show>
          </dl>
          <section class={styles.usage} aria-labelledby="turn-token-usage">
            <h3 id="turn-token-usage">Token usage</h3>
            <dl class={styles.tokenMetrics}>
              <div>
                <dt>New input</dt>
                <dd>{formatTokens(usage().inputTokens)}</dd>
              </div>
              <div>
                <dt>Cache write</dt>
                <dd>{formatTokens(usage().cacheCreationInputTokens)}</dd>
              </div>
              <div>
                <dt>Cache read</dt>
                <dd>{formatTokens(usage().cacheReadInputTokens)}</dd>
              </div>
              <div>
                <dt>Output</dt>
                <dd>{formatTokens(usage().outputTokens)}</dd>
              </div>
              <Show when={(usage().reasoningOutputTokens ?? 0) > 0}>
                <div>
                  <dt>Thinking</dt>
                  <dd>{formatTokens(usage().reasoningOutputTokens ?? 0)}</dd>
                </div>
              </Show>
            </dl>
          </section>
        </ModalDialog>
      </Show>
    </>
  );
}

export function SessionInvocationIcon(props: {
  turns: readonly TurnTiming[];
  model: string | null;
}) {
  const [open, setOpen] = createSignal(false);
  const totals = () =>
    props.turns.reduce(
      (total, turn) => {
        const usage = turn.result.usage;
        total.apiMs += turn.result.durationAPI * 1_000;
        total.costUSD += turn.result.totalCostUSD;
        total.durationMs += turn.result.duration * 1_000;
        total.inputTokens += usage.inputTokens;
        total.cacheWriteInputTokens += usage.cacheCreationInputTokens;
        total.cacheReadInputTokens += usage.cacheReadInputTokens;
        total.outputTokens += usage.outputTokens;
        total.reasoningOutputTokens += usage.reasoningOutputTokens ?? 0;
        total.userWaitMs += turn.waitMs ?? 0;
        if (turn.changeStat !== null) {
          total.changeStat.files += turn.changeStat.files;
          total.changeStat.added += turn.changeStat.added;
          total.changeStat.deleted += turn.changeStat.deleted;
          total.changeStat.binaryFiles += turn.changeStat.binaryFiles;
        }
        return total;
      },
      {
        apiMs: 0,
        cacheReadInputTokens: 0,
        cacheWriteInputTokens: 0,
        costUSD: 0,
        durationMs: 0,
        inputTokens: 0,
        outputTokens: 0,
        reasoningOutputTokens: 0,
        userWaitMs: 0,
        changeStat: {
          files: 0,
          added: 0,
          deleted: 0,
          binaryFiles: 0,
        },
      },
    );
  const models = () =>
    Array.from(
      new Set(
        props.turns.flatMap((turn) => {
          const model = turn.result.usage.reportedModel || turn.reportedModel || props.model;
          return model ? [model] : [];
        }),
      ),
    );
  const hasCompleteChangeStats = () =>
    props.turns.length > 0 && props.turns.every((turn) => turn.changeStat !== null);

  return (
    <>
      <button
        type="button"
        class={styles.trigger}
        onClick={() => setOpen(true)}
        aria-label="Session invocation details"
        title="Session invocation details"
        data-testid="session-invocation-trigger"
      >
        <InfoIcon width="13" height="13" aria-hidden="true" />
      </button>
      <Show when={open()}>
        <ModalDialog
          class={styles.dialog}
          onClose={() => setOpen(false)}
          data-testid="session-invocation-dialog"
        >
          <div class={styles.heading}>
            <h2>Session details</h2>
            <button
              type="button"
              class={styles.close}
              onClick={() => setOpen(false)}
              aria-label="Close session details"
              title="Close session details"
            >
              <CloseIcon width="18" height="18" aria-hidden="true" />
            </button>
          </div>
          <dl class={styles.metrics}>
            <Show when={models().length > 0}>
              <div>
                <dt>{models().length === 1 ? "Model" : "Models"}</dt>
                <dd>{models().join(", ")}</dd>
              </div>
            </Show>
            <div>
              <dt>Combined turn time</dt>
              <dd>{formatTotalDuration(totals().durationMs)}</dd>
            </div>
            <div>
              <dt>Combined API time</dt>
              <dd>{formatTotalDuration(totals().apiMs)}</dd>
            </div>
            <div>
              <dt>Time awaiting user response</dt>
              <dd>{formatTotalDuration(totals().userWaitMs)}</dd>
            </div>
            <Show when={hasCompleteChangeStats()}>
              <div>
                <dt>Generated change</dt>
                <dd>{formatChangeStat(totals().changeStat, true)}</dd>
              </div>
            </Show>
            <div>
              <dt>Cost</dt>
              <dd>{formatUSD(totals().costUSD)}</dd>
            </div>
          </dl>
          <section class={styles.usage} aria-labelledby="session-token-usage">
            <h3 id="session-token-usage">Token usage</h3>
            <dl class={styles.tokenMetrics}>
              <div>
                <dt>New input</dt>
                <dd>{formatTokens(totals().inputTokens)}</dd>
              </div>
              <div>
                <dt>Cache write</dt>
                <dd>{formatTokens(totals().cacheWriteInputTokens)}</dd>
              </div>
              <div>
                <dt>Cache read</dt>
                <dd>{formatTokens(totals().cacheReadInputTokens)}</dd>
              </div>
              <div>
                <dt>Output</dt>
                <dd>{formatTokens(totals().outputTokens)}</dd>
              </div>
              <Show when={totals().reasoningOutputTokens > 0}>
                <div>
                  <dt>Thinking</dt>
                  <dd>{formatTokens(totals().reasoningOutputTokens)}</dd>
                </div>
              </Show>
            </dl>
          </section>
        </ModalDialog>
      </Show>
    </>
  );
}
