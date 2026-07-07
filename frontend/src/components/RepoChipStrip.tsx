// Reusable repo chip strip with branch editing and add-repo dropdown.

import { createEffect, createSignal, For, Show, onCleanup } from "solid-js";
import { Portal } from "solid-js/web";

import type { BranchInfo, Repo } from "@sdk/types.gen";

import { listRepoBranches } from "../api";
import SearchableSelect from "./SearchableSelect";
import selectStyles from "./SearchableSelect.module.css";
import styles from "./RepoChipStrip.module.css";

export type RepoEntry = { path: string; branch: string };

interface Props {
  repos: () => Repo[];
  selectedRepos: () => RepoEntry[];
  onAdd: (path: string) => void;
  onRemove: (path: string) => void;
  onSetBranch: (path: string, branch: string) => void;
  availableRecent: () => Repo[];
  availableRest: () => Repo[];
  /** Show the clone button (default true). */
  showClone?: boolean;
  onClone?: () => void;
  "data-testid"?: string;
}

export default function RepoChipStrip(props: Props) {
  // Branch options, loaded lazily per repo when its picker opens.
  const [branchCache, setBranchCache] = createSignal<Record<string, BranchInfo[]>>({});
  // Add-repo dropdown open state.
  const [addOpen, setAddOpen] = createSignal(false);
  const [repoFilter, setRepoFilter] = createSignal("");

  let addRef: HTMLButtonElement | undefined;
  let dropdownRef: HTMLDivElement | undefined;

  function loadBranches(path: string) {
    if (branchCache()[path]) return;
    listRepoBranches(path)
      .then((r) => setBranchCache((c) => ({ ...c, [path]: r.branches })))
      .catch(() => {});
  }

  function defaultBranchLabel(path: string) {
    const base = props.repos().find((r) => r.path === path)?.baseBranch;
    const remote = base?.remote;
    const branch = base?.name ?? "main";
    return <><span class={selectStyles.optionMuted}>Default</span>{" "}({remote ? `${remote}/` : ""}{branch})</>;
  }

  function branchOptions(path: string) {
    return (branchCache()[path] ?? []).map((b) => ({
      value: b.name,
      label: <>{b.name}{b.remote && <span class={selectStyles.optionMuted}> ({b.remote})</span>}</>,
      search: b.name,
    }));
  }

  const filteredRecent = () => {
    const f = repoFilter().toLowerCase();
    const all = [...props.availableRecent()].sort((a, b) => a.path < b.path ? -1 : 1);
    return f ? all.filter((r) => r.path.toLowerCase().includes(f)) : all;
  };
  const filteredRest = () => {
    const f = repoFilter().toLowerCase();
    return f ? props.availableRest().filter((r) => r.path.toLowerCase().includes(f)) : props.availableRest();
  };

  // Reset filter when dropdown closes.
  createEffect(() => { if (!addOpen()) setRepoFilter(""); });

  // Close add-repo dropdown on Escape.
  const onAddKeyDown = (e: KeyboardEvent) => {
    if (e.key === "Escape") { setAddOpen(false); e.stopPropagation(); }
  };
  createEffect(() => {
    if (addOpen()) document.addEventListener("keydown", onAddKeyDown, true);
    else document.removeEventListener("keydown", onAddKeyDown, true);
    onCleanup(() => document.removeEventListener("keydown", onAddKeyDown, true));
  });

  // Close add-repo dropdown on outside click.
  const onAddClickOutside = (e: MouseEvent) => {
    const inTrigger = addRef?.contains(e.target as Node) ?? false;
    const inDropdown = dropdownRef?.contains(e.target as Node) ?? false;
    if (!inTrigger && !inDropdown) setAddOpen(false);
  };
  createEffect(() => {
    if (addOpen()) document.addEventListener("click", onAddClickOutside, true);
    else document.removeEventListener("click", onAddClickOutside, true);
    onCleanup(() => document.removeEventListener("click", onAddClickOutside, true));
  });
  // Position the add-repo portal dropdown below its trigger button.
  createEffect(() => {
    if (!addOpen() || !dropdownRef || !addRef) return;
    const r = addRef.getBoundingClientRect();
    const gap = 4;
    const margin = 8;
    const available = window.innerHeight - r.bottom - gap - margin;
    dropdownRef.style.top = `${r.bottom + gap}px`;
    dropdownRef.style.left = `${r.left}px`;
    dropdownRef.style.maxHeight = `${Math.min(available, 480)}px`;
  });

  return (
    <div class={styles.repoChips} data-testid={props["data-testid"]}>
      <For each={props.selectedRepos()}>
        {(entry) => (
          <span class={styles.repoChip}>
            <SearchableSelect
              class={styles.chipLabel}
              menuClass={styles.branchDropdown}
              ariaLabel={`Branch for ${entry.path}`}
              value={entry.branch}
              options={() => branchOptions(entry.path)}
              placeholder="Branch name…"
              emptyOption={{ value: "", label: defaultBranchLabel(entry.path) }}
              onOpen={() => loadBranches(entry.path)}
              onChange={(b) => props.onSetBranch(entry.path, b)}
              triggerLabel={
                <>{entry.path.split("/").pop()}
                  <Show when={entry.branch}>
                    <span class={styles.chipBranch}> · {entry.branch}</span>
                  </Show>
                </>
              }
              data-testid={`chip-label-${entry.path}`}
            />
            <button
              type="button"
              class={styles.chipRemove}
              onClick={() => props.onRemove(entry.path)}
              aria-label={`Remove ${entry.path}`}
              data-testid={`chip-remove-${entry.path}`}
            >×</button>
          </span>
        )}
      </For>
      <Show when={props.availableRecent().length > 0 || props.availableRest().length > 0}>
        <div class={styles.addRepoWrap}>
          <button
            ref={(el) => { addRef = el; }}
            type="button"
            class={styles.addRepoBtn}
            onClick={() => setAddOpen((v) => !v)}
            data-testid="add-repo-button"
            title="Add a repository"
          >+</button>
          <Show when={addOpen()}>
            <Portal>
            <div ref={(el) => { dropdownRef = el; }} class={styles.addRepoDropdown} data-testid="add-repo-dropdown">
              <input
                ref={(el) => setTimeout(() => el.focus(), 0)}
                type="text"
                class={styles.branchInput}
                placeholder="Filter repositories…"
                value={repoFilter()}
                onInput={(e) => setRepoFilter(e.currentTarget.value)}
              />
              <Show when={filteredRecent().length > 0}>
                <div class={styles.dropdownGroupLabel}>Recent</div>
                <For each={filteredRecent()}>
                  {(r) => (
                    <button type="button" class={styles.dropdownOption} onClick={() => { props.onAdd(r.path); setAddOpen(false); }}>
                      {r.path}
                    </button>
                  )}
                </For>
              </Show>
              <Show when={filteredRest().length > 0}>
                <Show when={filteredRecent().length > 0}>
                  <div class={styles.dropdownGroupLabel}>All repositories</div>
                </Show>
                <For each={filteredRest()}>
                  {(r) => (
                    <button type="button" class={styles.dropdownOption} onClick={() => { props.onAdd(r.path); setAddOpen(false); }}>
                      {r.path}
                    </button>
                  )}
                </For>
              </Show>
              <Show when={filteredRecent().length === 0 && filteredRest().length === 0}>
                <div class={styles.dropdownGroupLabel}>No matches</div>
              </Show>
            </div>
            </Portal>
          </Show>
        </div>
      </Show>
      <Show when={props.showClone !== false && props.onClone}>
        <button
          type="button"
          class={styles.cloneButton}
          onClick={() => props.onClone?.()}
          title="Clone a repository"
          data-testid="clone-toggle"
        >⎘</button>
      </Show>
    </div>
  );
}
