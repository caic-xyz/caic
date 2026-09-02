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
  harnessKeyShortcuts?: string;
  onHarnessCommit?: () => void;
  // Disambiguates aria-labels when two instances coexist, e.g. "Fork ".
  labelPrefix?: string;
}) {
  const label = (name: string) => `${props.labelPrefix ?? ""}${name}`;
  const moveHarness = (delta: number) => {
    const current = props.harnesses.findIndex((candidate) => candidate.name === props.harness);
    const next = Math.min(props.harnesses.length - 1, Math.max(0, current + delta));
    const nextHarness = props.harnesses[next];
    if (nextHarness) props.onHarness(nextHarness.name);
  };
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
          aria-keyshortcuts={props.harnessKeyShortcuts}
          data-testid={props.labelPrefix ? "fork-harness-select" : "harness-select"}
          title={props.harnessKeyShortcuts ? `Choose harness (${props.harnessKeyShortcuts})` : undefined}
          value={props.harness}
          onChange={(e) => props.onHarness(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && props.onHarnessCommit) {
              e.preventDefault();
              props.onHarnessCommit();
              return;
            }
            if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
            e.preventDefault();
            moveHarness(e.key === "ArrowDown" ? 1 : -1);
          }}
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
          ariaKeyShortcuts={props.harnesses.length > 1 ? undefined : props.harnessKeyShortcuts}
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
          data-testid={`${props.labelPrefix ? "fork-" : ""}effort-select`}
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
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          e.preventDefault();
          props.onChange(!props.checked);
        }}
      />
      {props.children}
    </label>
  );
}
