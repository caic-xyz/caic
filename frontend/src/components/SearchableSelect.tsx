// Reusable searchable select (combobox) with keyboard navigation and optional grouped options.

import { createEffect, createMemo, createSignal, For, Show, onCleanup, type JSX } from "solid-js";
import { Portal } from "solid-js/web";

import KeyboardArrowDown from "@material-symbols/svg-400/outlined/keyboard_arrow_down.svg?solid";

import styles from "./SearchableSelect.module.css";

export interface SearchableOption {
  value: string;
  label: JSX.Element;
  /** Lowercased text used for filtering; defaults to the string-cast label. */
  search?: string;
  /** Optional group label rendered before the first matching option in a group. */
  group?: string;
}

interface Props {
  value: string;
  options: () => SearchableOption[];
  ariaLabel: string;
  onChange: (value: string) => void;
  placeholder?: string;
  /** Pinned first option, e.g. "Default". Always shown at the top. */
  emptyOption?: SearchableOption;
  /** Overrides the trigger text; defaults to the selected option's label. */
  triggerLabel?: JSX.Element;
  hideCaret?: boolean;
  noOptionsLabel?: JSX.Element;
  disabled?: boolean;
  title?: string;
  class?: string;
  menuClass?: string;
  /** Fired when the popup opens, e.g. to lazily load options. */
  onOpen?: () => void;
  "data-testid"?: string;
  menuTestId?: string;
}

let idSeq = 0;

export default function SearchableSelect(props: Props) {
  const [open, setOpen] = createSignal(false);
  const [filter, setFilter] = createSignal("");
  const [active, setActive] = createSignal(0);

  const listboxId = `searchable-${++idSeq}`;
  let triggerRef: HTMLButtonElement | undefined;
  let inputRef: HTMLInputElement | undefined;
  let menuRef: HTMLDivElement | undefined;
  const optionRefs: (HTMLButtonElement | undefined)[] = [];

  const visibleOptions = createMemo(() => {
    const f = filter().toLowerCase();
    const opts = f
      ? props.options().filter((o) => (o.search ?? String(o.label)).toLowerCase().includes(f))
      : props.options();
    const base = (!f && props.emptyOption) ? [props.emptyOption] : [];
    return [...base, ...opts];
  });

  const selectedLabel = () => {
    if (props.value === "" && props.emptyOption) return props.emptyOption.label;
    const found = props.options().find((o) => o.value === props.value);
    return found ? found.label : (props.value || props.placeholder || "");
  };

  function openMenu() {
    if (props.disabled) return;
    setFilter("");
    const vis = visibleOptions();
    const idx = vis.findIndex((o) => o.value === props.value);
    setActive(idx >= 0 ? idx : 0);
    setOpen(true);
    props.onOpen?.();
    requestAnimationFrame(() => inputRef?.focus());
  }

  function closeMenu(focusTrigger = true) {
    setOpen(false);
    if (focusTrigger) triggerRef?.focus();
  }

  function commit(value: string) {
    props.onChange(value);
    closeMenu();
  }

  function move(delta: number) {
    const len = visibleOptions().length;
    if (len === 0) return;
    setActive((i) => Math.min(len - 1, Math.max(0, i + delta)));
  }

  function onClickOutside(e: MouseEvent) {
    if (menuRef?.contains(e.target as Node) || triggerRef?.contains(e.target as Node)) return;
    setOpen(false);
  }
  createEffect(() => {
    if (open()) document.addEventListener("mousedown", onClickOutside, true);
    else document.removeEventListener("mousedown", onClickOutside, true);
    onCleanup(() => document.removeEventListener("mousedown", onClickOutside, true));
  });

  function onDocKey(e: KeyboardEvent) {
    if (e.key === "Escape") { setOpen(false); e.stopPropagation(); }
  }
  createEffect(() => {
    if (open()) document.addEventListener("keydown", onDocKey, true);
    else document.removeEventListener("keydown", onDocKey, true);
    onCleanup(() => document.removeEventListener("keydown", onDocKey, true));
  });

  createEffect(() => {
    if (!open() || !menuRef || !triggerRef) return;
    const r = triggerRef.getBoundingClientRect();
    const gap = 4;
    const margin = 8;
    const available = window.innerHeight - r.bottom - gap - margin;
    menuRef.style.top = `${r.bottom + gap}px`;
    menuRef.style.left = `${r.left}px`;
    menuRef.style.maxHeight = `${Math.min(available, 360)}px`;
  });

  createEffect(() => {
    if (!open()) return;
    optionRefs[active()]?.scrollIntoView?.({ block: "nearest" });
  });

  function onTriggerKeyDown(e: KeyboardEvent) {
    if (props.disabled) return;
    if (e.key === "ArrowDown" || e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      openMenu();
    }
  }

  function onInputKeyDown(e: KeyboardEvent) {
    if (e.key === "ArrowDown") { e.preventDefault(); move(1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); move(-1); }
    else if (e.key === "Home") { e.preventDefault(); setActive(0); }
    else if (e.key === "End") { e.preventDefault(); setActive(Math.max(0, visibleOptions().length - 1)); }
    else if (e.key === "Enter") {
      e.preventDefault();
      const v = visibleOptions()[active()];
      if (v) commit(v.value);
    } else if (e.key === "Escape") {
      e.preventDefault();
      closeMenu();
    } else if (e.key === "Tab") {
      setOpen(false);
    }
  }

  return (
    <>
      <button
        ref={(el) => { triggerRef = el; }}
        type="button"
        class={`${styles.trigger} ${props.class ?? ""}`}
        aria-haspopup="listbox"
        aria-expanded={open()}
        aria-label={props.ariaLabel}
        disabled={props.disabled}
        title={props.title}
        data-testid={props["data-testid"]}
        onClick={() => (open() ? closeMenu(false) : openMenu())}
        onKeyDown={onTriggerKeyDown}
      >
        <span class={styles.triggerLabel}>{props.triggerLabel ?? selectedLabel()}</span>
        <Show when={!props.hideCaret}>
          <KeyboardArrowDown class={styles.caret} aria-hidden="true" />
        </Show>
      </button>
      <Show when={open()}>
        <Portal>
          <div
            ref={(el) => { menuRef = el; }}
            class={`${styles.menu} ${props.menuClass ?? ""}`}
            role="listbox"
            aria-label={props.ariaLabel}
            id={listboxId}
            data-testid={props.menuTestId}
          >
            <input
              ref={(el) => { inputRef = el; }}
              type="text"
              class={styles.input}
              placeholder={props.placeholder ?? "Search…"}
              value={filter()}
              aria-label={props.ariaLabel}
              role="combobox"
              aria-expanded={open()}
              aria-controls={listboxId}
              aria-activedescendant={visibleOptions()[active()] ? `${listboxId}-opt-${active()}` : undefined}
              onInput={(e) => { setFilter(e.currentTarget.value); setActive(0); }}
              onKeyDown={onInputKeyDown}
            />
            <For each={visibleOptions()}>
              {(opt, i) => (
                <>
                  <Show when={opt.group && opt.group !== visibleOptions()[i() - 1]?.group}>
                    <div class={styles.groupLabel}>{opt.group}</div>
                  </Show>
                  <button
                    ref={(el) => { optionRefs[i()] = el; }}
                    type="button"
                    id={`${listboxId}-opt-${i()}`}
                    class={`${styles.option}${i() === active() ? ` ${styles.optionActive}` : ""}${opt.value === props.value ? ` ${styles.optionSelected}` : ""}`}
                    role="option"
                    aria-selected={opt.value === props.value}
                    onMouseEnter={() => setActive(i())}
                    onClick={() => commit(opt.value)}
                  >
                    {opt.label}
                  </button>
                </>
              )}
            </For>
            <Show when={visibleOptions().length === 0 && props.noOptionsLabel}>
              <div class={styles.groupLabel}>{props.noOptionsLabel}</div>
            </Show>
          </div>
        </Portal>
      </Show>
    </>
  );
}
