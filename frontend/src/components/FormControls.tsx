// Shared form control components for task harness selects and capability toggles.

import { createEffect, createMemo, For, Show, splitProps, type JSX } from "solid-js";

import type { HarnessInfo } from "@sdk/types.gen";

import KeyboardArrowDown from "@material-symbols/svg-400/outlined/keyboard_arrow_down.svg?solid";

import SearchableSelect from "./SearchableSelect";
import styles from "./FormControls.module.css";

type ControlSelectProps = JSX.SelectHTMLAttributes<HTMLSelectElement>;

function classes(base: string | undefined, extra: string | undefined) {
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

// HarnessControls renders the harness/model/effort select triple. Reapply the
// harness value after option reordering, which can reset the native selection.
// Effort options mark their selected state for simultaneous option/value updates.
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
  harnessOptionLabel?: (harness: HarnessInfo) => string;
}) {
  let harnessSelect: HTMLSelectElement | undefined;
  createEffect(() => {
    const harnesses = props.harnesses;
    const harness = props.harness;
    if (harnesses.length > 1 && harnessSelect) harnessSelect.value = harness;
  });
  const label = (name: string) => `${props.labelPrefix ?? ""}${name}`;
  const moveHarness = (delta: number) => {
    const current = props.harnesses.findIndex((candidate) => candidate.name === props.harness);
    const next = Math.min(props.harnesses.length - 1, Math.max(0, current + delta));
    const nextHarness = props.harnesses[next];
    if (nextHarness) props.onHarness(nextHarness.name);
  };
  // Quota ordering and clock updates retain the selected harness's model menu.
  const selectedHarness = createMemo(() => props.harnesses.find((h) => h.name === props.harness));
  const modelOptions = createMemo(() =>
    (selectedHarness()?.models ?? []).map((model) => ({
      value: model.id,
      label: model.id as JSX.Element,
      search: model.id,
    })),
  );
  const efforts = () => {
    const model = selectedHarness()?.models.find((candidate) => candidate.id === props.model);
    return model?.effortOptions ?? [];
  };
  return (
    <>
      <Show when={props.harnesses.length > 1}>
        <ControlSelect
          ref={harnessSelect}
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
            {(h) => (
              <option value={h.name} selected={h.name === props.harness}>
                {props.harnessOptionLabel?.(h) ?? h.name}
              </option>
            )}
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
          <option value="" selected={props.effort === ""}>
            Default effort
          </option>
          <For each={efforts()}>
            {(e) => (
              <option value={e} selected={e === props.effort}>
                {e}
              </option>
            )}
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
