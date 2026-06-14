// Shared form control components for task harness selects and capability toggles.
import { For, Show, splitProps, type JSX } from "solid-js";
import type { Harness, HarnessInfo } from "@sdk/types.gen";
import { effortOptions } from "../effortOptions";
import styles from "./FormControls.module.css";

type ControlSelectProps = JSX.SelectHTMLAttributes<HTMLSelectElement>;

function classes(base: string, extra: string | undefined) {
  return extra ? `${base} ${extra}` : base;
}

export function ControlSelect(props: ControlSelectProps) {
  const [local, rest] = splitProps(props, ["class", "children"]);

  return (
    <select {...rest} class={classes(styles.select, local.class)}>
      {local.children}
    </select>
  );
}

// HarnessControls renders the harness/model/effort select triple. Options mark
// their own `selected` state so the painted value can't desync from the signal
// when options and value update in the same flush (async load, dialog open).
export function HarnessControls(props: {
  harnesses: HarnessInfo[];
  harness: string;
  model: string;
  effort: string;
  onHarness: (harness: string) => void;
  onModel: (model: string) => void;
  onEffort: (effort: string) => void;
  // Disambiguates aria-labels when two instances coexist, e.g. "Fork ".
  labelPrefix?: string;
}) {
  const label = (name: string) => `${props.labelPrefix ?? ""}${name}`;
  const models = () => props.harnesses.find((h) => h.name === props.harness)?.models ?? [];
  const efforts = () => effortOptions(props.harness as Harness);
  return (
    <>
      <Show when={props.harnesses.length > 1}>
        <ControlSelect
          aria-label={label("Harness")}
          value={props.harness}
          onChange={(e) => props.onHarness(e.currentTarget.value)}
        >
          <For each={props.harnesses}>
            {(h) => <option value={h.name} selected={h.name === props.harness}>{h.name}</option>}
          </For>
        </ControlSelect>
      </Show>
      <Show when={models().length > 0}>
        <ControlSelect
          aria-label={label("Model")}
          value={props.model}
          onChange={(e) => props.onModel(e.currentTarget.value)}
        >
          <option value="" selected={props.model === ""}>Default model</option>
          <For each={models()}>
            {(m) => <option value={m} selected={m === props.model}>{m}</option>}
          </For>
        </ControlSelect>
      </Show>
      <Show when={efforts().length > 0}>
        <ControlSelect
          aria-label={label("Effort")}
          value={props.effort}
          onChange={(e) => props.onEffort(e.currentTarget.value)}
        >
          <option value="" selected={props.effort === ""}>Default effort</option>
          <For each={efforts()}>
            {(e) => <option value={e} selected={e === props.effort}>{e}</option>}
          </For>
        </ControlSelect>
      </Show>
    </>
  );
}

export function ToggleChip(props: {
  checked: boolean;
  children: JSX.Element;
  disabled?: boolean;
  title: string;
  onChange: (checked: boolean) => void;
}) {
  return (
    <label class={styles.toggleChip} title={props.title}>
      <input
        type="checkbox"
        checked={props.checked}
        disabled={props.disabled}
        aria-label={props.title}
        onChange={(e) => props.onChange(e.currentTarget.checked)}
      />
      {props.children}
    </label>
  );
}
