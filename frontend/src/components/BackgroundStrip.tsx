// Lists a task's running commands, background agents, and watches above the prompt input.

import { createMemo, For, Show } from "solid-js";

import type { BackgroundItem, BackgroundKind } from "../backgroundActivity";
import { createLiveDuration } from "../liveDuration";
import { formatTimingDuration } from "../timing";
import styles from "./BackgroundStrip.module.css";

const kindName: Record<BackgroundKind, string> = { agent: "Agent", command: "Command", watch: "Watch" };

// BackgroundStrip renders nothing when no item runs. Activating a chip asks
// the owner to reveal the transcript element that started the item.
export default function BackgroundStrip(props: { items: BackgroundItem[]; onReveal: (item: BackgroundItem) => void }) {
  const byID = createMemo(() => new Map(props.items.map((item) => [`${item.kind}:${item.id}`, item])));
  const keys = createMemo(() => [...byID().keys()]);
  return (
    <Show when={keys().length > 0}>
      <section class={styles.strip} aria-label="Background work" data-testid="background-strip">
        <div class={styles.scroller}>
          <ul class={styles.list}>
            <For each={keys()}>
              {(key) => (
                <Show when={byID().get(key)}>{(item) => <Chip item={item()} onReveal={props.onReveal} />}</Show>
              )}
            </For>
          </ul>
        </div>
      </section>
    </Show>
  );
}

function Chip(props: { item: BackgroundItem; onReveal: (item: BackgroundItem) => void }) {
  const elapsed = createLiveDuration(
    () => props.item.startedAt,
    () => null,
    () => true,
  );
  return (
    <li>
      <button
        type="button"
        class={styles.chip}
        data-testid="background-chip"
        data-kind={props.item.kind}
        title={`${kindName[props.item.kind]}: ${props.item.label}`}
        onClick={() => props.onReveal(props.item)}
        // Keyboard focus can land on a chip that the row clips.
        onFocus={(event) => event.currentTarget.scrollIntoView({ block: "nearest", inline: "nearest" })}
      >
        <span class={styles.kind}>{kindName[props.item.kind]}</span>{" "}
        <span class={styles.label}>{props.item.label}</span>
        <Show when={elapsed() !== null}>
          {" "}
          <span class={styles.duration}>{formatTimingDuration(elapsed() ?? 0)}</span>
        </Show>
      </button>
    </li>
  );
}
