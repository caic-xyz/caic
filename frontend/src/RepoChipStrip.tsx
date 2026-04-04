// Reusable repo chip strip with branch editing and add-repo dropdown.
import { createEffect, createSignal, For, Show, onCleanup } from "solid-js";
import { Portal } from "solid-js/web";
import type { BranchInfo, Repo } from "@sdk/types.gen";
import { listRepoBranches } from "./api";
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
  // Branch dropdown state.
  const [editingPath, setEditingPath] = createSignal<string | null>(null);
  const [editingBranches, setEditingBranches] = createSignal<BranchInfo[]>([]);
  const [branchTriggerRect, setBranchTriggerRect] = createSignal<DOMRect | null>(null);
  const [branchFilter, setBranchFilter] = createSignal("");
  // Add-repo dropdown open state.
  const [addOpen, setAddOpen] = createSignal(false);

  let addRef: HTMLButtonElement | undefined;
  let dropdownRef: HTMLDivElement | undefined;
  let branchDropdownRef: HTMLDivElement | undefined;

  function startEditBranch(path: string, triggerRect: DOMRect) {
    if (editingPath() === path) { setEditingPath(null); return; }
    setEditingPath(path);
    setBranchTriggerRect(triggerRect);
    setBranchFilter(props.selectedRepos().find((r) => r.path === path)?.branch ?? "");
    setEditingBranches([]);
    listRepoBranches(path).then((r) => setEditingBranches(r.branches)).catch(() => {});
  }

  function commitBranch(branch: string) {
    const path = editingPath();
    if (!path) return;
    props.onSetBranch(path, branch);
    setEditingPath(null);
  }

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
  // Close branch dropdown on outside click.
  const onBranchClickOutside = (e: MouseEvent) => {
    if (branchDropdownRef?.contains(e.target as Node)) return;
    setEditingPath(null);
  };
  createEffect(() => {
    if (editingPath()) document.addEventListener("click", onBranchClickOutside, true);
    else document.removeEventListener("click", onBranchClickOutside, true);
    onCleanup(() => document.removeEventListener("click", onBranchClickOutside, true));
  });
  // Position the branch portal dropdown below the clicked chip.
  createEffect(() => {
    if (!editingPath() || !branchDropdownRef) return;
    const r = branchTriggerRect();
    if (!r) return;
    const gap = 4;
    const margin = 8;
    const available = window.innerHeight - r.bottom - gap - margin;
    branchDropdownRef.style.top = `${r.bottom + gap}px`;
    branchDropdownRef.style.left = `${r.left}px`;
    branchDropdownRef.style.maxHeight = `${Math.min(available, 360)}px`;
  });

  return (
    <div class={styles.repoChips} data-testid={props["data-testid"]}>
      <Show when={editingPath()}>
        <Portal>
          <div ref={(el) => { branchDropdownRef = el; }} class={styles.branchDropdown}>
            <input
              ref={(el) => setTimeout(() => el.focus(), 0)}
              type="text"
              class={styles.branchInput}
              placeholder="Branch name…"
              value={branchFilter()}
              onInput={(e) => setBranchFilter(e.currentTarget.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") { commitBranch(branchFilter()); e.preventDefault(); }
                if (e.key === "Escape") { setEditingPath(null); e.preventDefault(); }
              }}
            />
            <Show when={!branchFilter()}>
              <button type="button" class={styles.dropdownOption}
                onClick={() => commitBranch("")}
              >
                {(() => {
                  const base = props.repos().find((r) => r.path === editingPath())?.baseBranch;
                  const remote = base?.remote;
                  const branch = base?.name ?? "main";
                  return <><span class={styles.dropdownOptionMuted}>Default</span>{" "}({remote ? `${remote}/` : ""}{branch})</>;
                })()}
              </button>
            </Show>
            <For each={editingBranches().filter((b) => {
              const f = branchFilter().toLowerCase();
              return !f || b.name.toLowerCase().includes(f);
            })}>
              {(b) => (
                <button type="button" class={`${styles.dropdownOption}${props.selectedRepos().find((r) => r.path === editingPath())?.branch === b.name ? ` ${styles.dropdownOptionActive}` : ""}`}
                  onClick={() => commitBranch(b.name)}
                >{b.name}{b.remote && <span class={styles.dropdownOptionMuted}> ({b.remote})</span>}</button>
              )}
            </For>
          </div>
        </Portal>
      </Show>
      <For each={props.selectedRepos()}>
        {(entry) => (
          <span class={styles.repoChip}>
            <button
              type="button"
              class={`${styles.chipLabel} ${editingPath() === entry.path ? styles.chipLabelActive : ""}`}
              onClick={(e) => startEditBranch(entry.path, (e.currentTarget as HTMLButtonElement).getBoundingClientRect())}
              title="Click to set branch"
              data-testid={`chip-label-${entry.path}`}
            >
              {entry.path.split("/").pop()}
              <Show when={entry.branch}>
                <span class={styles.chipBranch}> · {entry.branch}</span>
              </Show>
            </button>
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
              <Show when={props.availableRecent().length > 0}>
                <div class={styles.dropdownGroupLabel}>Recent</div>
                <For each={[...props.availableRecent()].sort((a, b) => a.path < b.path ? -1 : 1)}>
                  {(r) => (
                    <button type="button" class={styles.dropdownOption} onClick={() => { props.onAdd(r.path); setAddOpen(false); }}>
                      {r.path}
                    </button>
                  )}
                </For>
              </Show>
              <Show when={props.availableRest().length > 0}>
                <Show when={props.availableRecent().length > 0}>
                  <div class={styles.dropdownGroupLabel}>All repositories</div>
                </Show>
                <For each={props.availableRest()}>
                  {(r) => (
                    <button type="button" class={styles.dropdownOption} onClick={() => { props.onAdd(r.path); setAddOpen(false); }}>
                      {r.path}
                    </button>
                  )}
                </For>
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
