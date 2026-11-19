// Lists a task's running commands, background agents by kind, and watches above the prompt input, each behind its kind's icon.

import { createMemo, For, Show } from "solid-js";

import { genericKindNames, type BackgroundItem } from "../backgroundActivity";
import { createLiveDuration } from "../liveDuration";
import { formatTimingDuration } from "../timing";
import BackgroundKindIcon from "./BackgroundKindIcon";
import styles from "./BackgroundStrip.module.css";

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
  // A harness can name an agent only by its kind; the chip then says it once.
  const label = () => (props.item.label === props.item.kindName ? "" : props.item.label);
  // The icon shows a generic kind; its name remains for assistive technology.
  const generic = () => props.item.kindName === genericKindNames[props.item.kind];
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
        title={label() ? `${props.item.kindName}: ${label()}` : props.item.kindName}
        onClick={() => props.onReveal(props.item)}
        // Keyboard focus can land on a chip that the narrow single row clips;
        // wrapped chips are already in view, so this is then a no-op.
        onFocus={(event) => event.currentTarget.scrollIntoView({ block: "nearest", inline: "nearest" })}
      >
        <BackgroundKindIcon kind={props.item.kind} class={styles.icon} />
        <span class={generic() ? styles.visuallyHidden : styles.kind}>{props.item.kindName}</span>
        <Show when={label()}>
          {" "}
          <span class={styles.label}>{label()}</span>
        </Show>
        <Show when={elapsed() !== null}>
          {" "}
          <span class={styles.duration}>{formatTimingDuration(elapsed() ?? 0)}</span>
        </Show>
      </button>
    </li>
  );
}
