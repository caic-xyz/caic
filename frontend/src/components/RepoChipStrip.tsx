// Reusable repo chip strip with branch editing and add-repo dropdown.

import { createSignal, For, Show } from "solid-js";

import type { BranchInfo, Repo } from "@sdk/types.gen";

import { listRepoBranches } from "../api";
import SearchableSelect, { type SearchableOption } from "./SearchableSelect";
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

  const addRepoOptions = (): SearchableOption[] => {
    const recent = [...props.availableRecent()]
      .sort((a, b) => a.path < b.path ? -1 : 1)
      .map((r) => ({ value: r.path, label: r.path, search: r.path, group: "Recent" }));
    const rest = props.availableRest()
      .map((r) => ({ value: r.path, label: r.path, search: r.path, group: recent.length > 0 ? "All repositories" : undefined }));
    return [...recent, ...rest];
  };

  return (
    <div class={styles.repoChips} data-testid={props["data-testid"]}>
      <For each={props.selectedRepos()}>
        {(entry) => (
          <span class={styles.repoChip}>
            <SearchableSelect
              class={styles.chipLabel}
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
          <SearchableSelect
            class={styles.addRepoBtn}
            menuClass={styles.addRepoDropdown}
            ariaLabel="Add a repository"
            ariaKeyShortcuts="R"
            value=""
            options={addRepoOptions}
            placeholder="Filter repositories…"
            triggerLabel="+"
            hideCaret
            noOptionsLabel="No matches"
            onChange={props.onAdd}
            title="Add a repository (R)"
            data-testid="add-repo-button"
            menuTestId="add-repo-dropdown"
          />
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
