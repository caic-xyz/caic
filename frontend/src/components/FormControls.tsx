// Shared form control components for task harness selects and capability toggles.

import { For, Show, splitProps, type JSX } from "solid-js";

import type { HarnessInfo } from "@sdk/types.gen";

import KeyboardArrowDown from "@material-symbols/svg-400/outlined/keyboard_arrow_down.svg?solid";

import SearchableSelect from "./SearchableSelect";
import styles from "./FormControls.module.css";

type ControlSelectProps = JSX.SelectHTMLAttributes<HTMLSelectElement>;

function classes(base: string, extra: string | undefined) {
  return extra ? `${base} ${extra}` : base;
}

export function ControlSelect(props: ControlSelectProps) {
  const [local, rest] = splitProps(props, ["class", "children"]);

  return (
    <span class={styles.selectWrap}>
      <select {...rest} class={classes(styles.select, local.class)}>
        {local.children}
      </select>
      <KeyboardArrowDown class={styles.selectCaret} aria-hidden="true" />
    </span>
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
  const modelOptions = () => (props.harnesses.find((h) => h.name === props.harness)?.models ?? [])
    .map((model) => ({ value: model.id, label: model.id as JSX.Element, search: model.id }));
  const efforts = () => {
    const harness = props.harnesses.find((h) => h.name === props.harness);
    const model = harness?.models.find((candidate) => candidate.id === props.model);
    return model?.effortOptions ?? [];
  };
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
      <Show when={modelOptions().length > 0}>
        <SearchableSelect
          class={styles.selectTrigger}
          ariaLabel={label("Model")}
          value={props.model}
          options={modelOptions}
          placeholder="Search models…"
          emptyOption={{ value: "", label: "Default model" }}
          onChange={props.onModel}
          data-testid={`${props.labelPrefix ? "fork-" : ""}model-select`}
        />
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
