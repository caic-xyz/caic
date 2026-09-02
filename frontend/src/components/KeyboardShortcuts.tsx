// Global keyboard navigation and the discoverable keyboard-shortcuts reference dialog.

import { For, onCleanup, onMount, Show } from "solid-js";

import { useAppState } from "../AppState";
import { taskPathForTask } from "../taskPath";
import { confirmTaskAction } from "./TaskCard";
import ModalDialog from "./ModalDialog";
import styles from "./KeyboardShortcuts.module.css";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

const navigationShortcutsAfterModelSettings = [
  { keys: "↓ / Shift + ↓", action: "Select the next task" },
  { keys: "↑ / Shift + ↑", action: "Select the previous task" },
  { keys: "Tab", action: "Move from a task card to its prompt" },
  { keys: "Shift + Tab", action: "Return from a prompt to its task card" },
  { keys: "Esc", action: "Close a dialog, otherwise focus the new-task prompt" },
  { keys: "? / F1", action: "Show keyboard shortcuts" },
];

const promptShortcuts = [
  { keys: "Shift + ↑ / ↓", action: "Switch tasks and keep typing" },
  { keys: "Esc", action: "Focus the new-task prompt" },
];

const taskActionShortcuts = [
  { keys: "Shift + Delete", action: "Purge the selected task" },
];

function isEditing(target: EventTarget | null): boolean {
  return target instanceof HTMLElement
    && target.closest("input, textarea, select, [contenteditable='true'], [role='textbox']") !== null;
}

export default function KeyboardShortcuts(props: Props) {
  const s = useAppState();
  let shortcutOpener: HTMLElement | null = null;
  let shortcutOpenerTaskId: string | undefined;

  const navigationShortcuts = () => {
    const harnesses = s.harnesses();
    const f3 = harnesses.length > 1
      ? { keys: "F3", action: "Focus harness for the new task" }
      : harnesses.find((harness) => harness.name === s.selectedHarness())?.models.length
        ? { keys: "F3", action: "Focus model for the new task" }
        : s.runtimes().length > 1
          ? { keys: "F3", action: "Focus runtime for the new task" }
          : null;
    return [
      { keys: "F2", action: "Focus repositories for the new task" },
      ...(f3 ? [f3] : []),
      ...navigationShortcutsAfterModelSettings,
    ];
  };

  function restoreShortcutFocus() {
    if (shortcutOpener?.isConnected) {
      shortcutOpener.focus();
      return;
    }
    taskCards().find((card) => card.dataset.taskId === shortcutOpenerTaskId)?.focus();
  }

  function focusNewTaskControl(selector: string) {
    s.navigate("/");
    requestAnimationFrame(() => {
      const el = document.querySelector<HTMLElement>(selector);
      el?.focus();
    });
  }

  function openTaskFromCard(card: HTMLElement, focusPrompt: boolean) {
    const task = s.tasks().find((candidate) => candidate.id === card.dataset.taskId);
    if (!task) return;
    s.navigate(taskPathForTask(task));
    if (!focusPrompt) card.focus();
    requestAnimationFrame(() => {
      if (focusPrompt) {
        document.querySelector<HTMLElement>("[data-testid='task-detail-prompt']")?.focus();
        return;
      }
      const currentCard = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"))
        .find((el) => el.dataset.taskId === task.id);
      currentCard?.focus();
    });
  }

  const taskCards = () => Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"));

  function navigateTask(delta: number, focusPrompt: boolean, currentTaskId: string | null | undefined = s.selectedId()) {
    const cards = taskCards();
    if (cards.length === 0) return;
    const current = cards.findIndex((el) => el.dataset.taskId === currentTaskId);
    const next = current === -1
      ? (delta > 0 ? 0 : cards.length - 1)
      : (current + delta + cards.length) % cards.length;
    openTaskFromCard(cards[next], focusPrompt);
  }

  function purgeSelectedTask() {
    const task = s.selectedTask();
    if (!task || new Set(["failed", "purged", "purging", "stopping"]).has(task.state)) return;
    if (confirmTaskAction("Purge", task.title, task.repos?.[0]?.branch ?? "")) s.handlePurge(task.id);
  }

  onMount(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey) return;
      if (document.querySelector("dialog[open]")) return;

      const target = event.target instanceof HTMLElement ? event.target : null;
      const taskPrompt = target?.closest("[data-testid='task-detail-prompt'], [data-testid='prompt-input']");
      const focusedCard = target?.matches("[data-task-id]") ? target : null;
      if (event.key === "Escape") {
        event.preventDefault();
        focusNewTaskControl("[data-testid='prompt-input']");
        return;
      }
      if (focusedCard && (event.key === "ArrowDown" || event.key === "ArrowUp")) {
        event.preventDefault();
        navigateTask(event.key === "ArrowDown" ? 1 : -1, false, focusedCard.dataset.taskId);
        return;
      }
      if (event.shiftKey
        && (taskPrompt || !isEditing(target))
        && (event.key === "ArrowDown" || event.key === "ArrowUp")) {
        event.preventDefault();
        navigateTask(event.key === "ArrowDown" ? 1 : -1, true);
        return;
      }
      if (event.key === "Tab" && !event.shiftKey && focusedCard) {
        event.preventDefault();
        openTaskFromCard(focusedCard, true);
        return;
      }
      if (event.key === "F1" || (event.key === "?" && !isEditing(target))) {
        event.preventDefault();
        shortcutOpener = target;
        shortcutOpenerTaskId = target?.closest<HTMLElement>("[data-task-id]")?.dataset.taskId;
        props.onOpenChange(true);
        return;
      }
      if (event.key === "F2") {
        event.preventDefault();
        focusNewTaskControl("[data-testid='repo-chips'] [data-testid^='chip-label-'], [data-testid='add-repo-button']");
        return;
      }
      if (event.key === "F3") {
        event.preventDefault();
        focusNewTaskControl("[data-testid='harness-select'], [data-testid='model-select'], [data-testid='effort-select'], [data-testid='runtime-select']");
        return;
      }
      if (event.key === "Delete" && event.shiftKey) {
        event.preventDefault();
        purgeSelectedTask();
        return;
      }
      if (isEditing(target)) return;
      if (event.shiftKey) return;

      if (event.key === "ArrowDown") {
        event.preventDefault();
        navigateTask(1, false);
      } else if (event.key === "ArrowUp") {
        event.preventDefault();
        navigateTask(-1, false);
      }
    };

    document.addEventListener("keydown", onKeyDown);
    onCleanup(() => document.removeEventListener("keydown", onKeyDown));
  });

  return (
    <Show when={props.open}>
      <ModalDialog
        class={styles.dialog}
        onClose={() => props.onOpenChange(false)}
        restoreFocus={restoreShortcutFocus}
        data-testid="keyboard-shortcuts-dialog"
      >
        <h2 class={styles.title}>Keyboard shortcuts</h2>
        <p class={styles.intro}>Move between task cards and prompts without leaving the keyboard. Local menus and dialogs handle Escape first.</p>
        <ShortcutSection title="Navigation" shortcuts={navigationShortcuts()} />
        <ShortcutSection title="While typing" shortcuts={promptShortcuts} />
        <ShortcutSection title="Task actions" shortcuts={taskActionShortcuts} />
        <button type="button" class={styles.closeButton} onClick={() => props.onOpenChange(false)}>Close</button>
      </ModalDialog>
    </Show>
  );
}

function ShortcutSection(props: { title: string; shortcuts: { keys: string; action: string }[] }) {
  return (
    <section class={styles.section}>
      <h3 class={styles.sectionTitle}>{props.title}</h3>
      <dl class={styles.shortcutList}>
        <For each={props.shortcuts}>
          {(shortcut) => (
            <div class={styles.shortcutRow}>
              <dt class={styles.keys}><kbd>{shortcut.keys}</kbd></dt>
              <dd class={styles.action}>{shortcut.action}</dd>
            </div>
          )}
        </For>
      </dl>
    </section>
  );
}
