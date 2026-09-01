// Shared task actions menu used by task details and task-card context menus.

import { Match, Show, Switch, type JSX } from "solid-js";
import BlockIcon from "@material-symbols/svg-400/outlined/block.svg?solid";
import CompressIcon from "@material-symbols/svg-400/outlined/compress.svg?solid";
import DeleteIcon from "@material-symbols/svg-400/outlined/delete.svg?solid";
import ForkIcon from "@material-symbols/svg-400/outlined/fork_right.svg?solid";
import RestartIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";
import StopIcon from "@material-symbols/svg-400/outlined/stop_circle.svg?solid";
import SyncIcon from "@material-symbols/svg-400/outlined/sync.svg?solid";

import GitHubIcon from "./github.svg?solid";
import GitLabIcon from "./gitlab.svg?solid";
import styles from "./TaskActionsMenu.module.css";

interface TaskActionsMenuProps {
  class?: string;
  style?: JSX.CSSProperties;
  menuRef?: (element: HTMLDivElement) => void;
  forge?: string;
  forgePR?: number;
  baseBranch: string;
  active: boolean;
  recoverable: boolean;
  waiting: boolean;
  purging: boolean;
  supportsCompact?: boolean;
  canFork: boolean;
  onSync: () => void;
  onSyncDefault: () => void;
  onStop: () => void;
  onRevive: () => void;
  onPurge: () => void;
  onCompact: () => void;
  onFork: () => void;
}

export default function TaskActionsMenu(props: TaskActionsMenuProps) {
  return (
    <div
      ref={(element) => props.menuRef?.(element)}
      class={`${styles.menu} ${props.class ?? ""}`}
      style={props.style}
      role="menu"
    >
      <button
        type="button"
        role="menuitem"
        class={`${styles.item} ${props.purging ? styles.disabled : ""}`}
        disabled={props.purging}
        onClick={() => props.onSync()}
      >
        <Switch fallback={<SyncIcon width="1em" height="1em" />}>
          <Match when={props.forge === "github"}>
            <GitHubIcon width="1em" height="1em" />
          </Match>
          <Match when={props.forge === "gitlab"}>
            <GitLabIcon width="1em" height="1em" />
          </Match>
        </Switch>
        {props.forge ? (props.forgePR ? "Push" : "Create PR") : "Push"}
      </button>
      <button
        type="button"
        role="menuitem"
        class={`${styles.item} ${props.purging ? styles.disabled : ""}`}
        disabled={props.purging}
        onClick={() => props.onSyncDefault()}
      >
        <SyncIcon width="1em" height="1em" />
        Push to {props.baseBranch}
      </button>
      <Show when={props.active}>
        <button
          type="button"
          role="menuitem"
          class={`${styles.item} ${styles.danger}`}
          onClick={() => props.onStop()}
        >
          <StopIcon width="1em" height="1em" />
          Stop
        </button>
      </Show>
      <Show when={props.recoverable}>
        <button type="button" role="menuitem" class={styles.item} onClick={() => props.onRevive()}>
          <RestartIcon width="1em" height="1em" />
          Revive
        </button>
      </Show>
      <Show when={props.active || props.recoverable}>
        <button
          type="button"
          role="menuitem"
          class={`${styles.item} ${styles.danger}`}
          onClick={() => props.onPurge()}
        >
          <DeleteIcon width="1em" height="1em" />
          Purge
        </button>
      </Show>
      <button type="button" role="menuitem" class={`${styles.item} ${styles.disabled}`} disabled>
        <BlockIcon width="1em" height="1em" />
        Clear context
      </button>
      <Show when={props.supportsCompact}>
        <button
          type="button"
          role="menuitem"
          class={`${styles.item} ${!props.waiting ? styles.disabled : ""}`}
          disabled={!props.waiting}
          onClick={() => props.onCompact()}
        >
          <CompressIcon width="1em" height="1em" />
          Compact context
        </button>
      </Show>
      <Show when={props.canFork}>
        <button type="button" role="menuitem" class={styles.item} onClick={() => props.onFork()}>
          <ForkIcon width="1em" height="1em" />
          Fork
        </button>
      </Show>
    </div>
  );
}
