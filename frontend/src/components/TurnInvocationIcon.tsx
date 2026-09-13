// TurnInvocationIcon opens per-turn invocation details from a result card.

import { createSignal, Show } from "solid-js";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import InfoIcon from "@material-symbols/svg-400/outlined/info.svg?solid";

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
        <ModalDialog class={styles.dialog} onClose={() => setOpen(false)} data-testid="turn-invocation-dialog">
          <div class={styles.heading}>
            <h2>Turn details</h2>
            <button type="button" class={styles.close} onClick={() => setOpen(false)} aria-label="Close turn details" title="Close turn details">
              <CloseIcon width="18" height="18" aria-hidden="true" />
            </button>
          </div>
          <dl class={styles.metrics}>
            <Show when={model()} keyed>
              {(name) => <div><dt>Model</dt><dd>{name}</dd></div>}
            </Show>
            <div><dt>Turn time</dt><dd>{formatTimingDuration(result().duration * 1_000)}</dd></div>
            <Show when={result().durationAPI > 0}>
              <div><dt>API time</dt><dd>{formatTimingDuration(result().durationAPI * 1_000)}</dd></div>
            </Show>
            <Show when={props.turn.waitMs !== null && props.turn.waitMs > 0}>
              <div><dt>User wait</dt><dd>{formatTimingDuration(props.turn.waitMs ?? 0)}</dd></div>
            </Show>
            <Show when={result().totalCostUSD > 0}>
              <div><dt>Cost</dt><dd>{formatUSD(result().totalCostUSD)}</dd></div>
            </Show>
          </dl>
          <section class={styles.usage} aria-labelledby="turn-token-usage">
            <h3 id="turn-token-usage">Token usage</h3>
            <dl class={styles.tokenMetrics}>
              <div><dt>New input</dt><dd>{formatTokens(usage().inputTokens)}</dd></div>
              <div><dt>Cache write</dt><dd>{formatTokens(usage().cacheCreationInputTokens)}</dd></div>
              <div><dt>Cache read</dt><dd>{formatTokens(usage().cacheReadInputTokens)}</dd></div>
              <div><dt>Output</dt><dd>{formatTokens(usage().outputTokens)}</dd></div>
              <Show when={(usage().reasoningOutputTokens ?? 0) > 0}>
                <div><dt>Thinking</dt><dd>{formatTokens(usage().reasoningOutputTokens ?? 0)}</dd></div>
              </Show>
            </dl>
          </section>
        </ModalDialog>
      </Show>
    </>
  );
}
