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

const globalShortcuts = [
  { keys: "N", action: "Start a new task" },
  { keys: "R", action: "Choose a repository" },
  { keys: "/", action: "Focus the active prompt" },
  { keys: "J / ↓", action: "Select the next task" },
  { keys: "K / ↑", action: "Select the previous task" },
  { keys: "Home / End", action: "Select the first or last task" },
  { keys: "Tab", action: "Move from a task card to its prompt" },
  { keys: "Shift + Tab", action: "Return from a prompt to its task card" },
  { keys: "Shift + Delete", action: "Purge the selected task" },
  { keys: "? / F1", action: "Show keyboard shortcuts" },
];

const promptShortcuts = [
  { keys: "Shift + ↑ / ↓", action: "Switch tasks and keep typing" },
  { keys: "Esc", action: "Return to the selected task card" },
];

const pickerShortcuts = [
  { keys: "Type", action: "Filter repositories" },
  { keys: "↑ / ↓", action: "Move through repositories" },
  { keys: "Enter", action: "Choose the highlighted repository" },
  { keys: "Esc", action: "Close the picker or menu first" },
];

function isEditing(target: EventTarget | null): boolean {
  return target instanceof HTMLElement
    && target.closest("input, textarea, select, [contenteditable='true'], [role='textbox']") !== null;
}

export default function KeyboardShortcuts(props: Props) {
  const s = useAppState();
  let shortcutOpener: HTMLElement | null = null;
  let shortcutOpenerTaskId: string | undefined;

  function restoreShortcutFocus() {
    if (shortcutOpener?.isConnected) {
      shortcutOpener.focus();
      return;
    }
    taskCards().find((card) => card.dataset.taskId === shortcutOpenerTaskId)?.focus();
  }

  function openNewTaskControl(selector: string, click: boolean) {
    s.navigate("/");
    requestAnimationFrame(() => {
      const el = document.querySelector<HTMLElement>(selector);
      if (click && el instanceof HTMLButtonElement) el.click();
      else el?.focus();
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

  function navigateTask(delta: number, focusPrompt: boolean) {
    const cards = taskCards();
    if (cards.length === 0) return;
    const current = cards.findIndex((el) => el.dataset.taskId === s.selectedId());
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
      const detailPrompt = target?.closest("[data-testid='task-detail-prompt']");
      const taskPrompt = detailPrompt ?? target?.closest("[data-testid='prompt-input']");
      const focusedCard = target?.matches("[data-task-id]") ? target : null;
      const activeDetailPrompt = document.querySelector<HTMLElement>("[data-testid='task-detail-prompt']");
      const inactiveNewTaskPrompt = target?.closest("[data-testid='prompt-input']") && activeDetailPrompt;

      if (event.key === "/" && !event.shiftKey && inactiveNewTaskPrompt) {
        event.preventDefault();
        activeDetailPrompt.focus();
        return;
      }
      if (event.shiftKey && taskPrompt && (event.key === "ArrowDown" || event.key === "ArrowUp")) {
        event.preventDefault();
        navigateTask(event.key === "ArrowDown" ? 1 : -1, true);
        return;
      }
      if (event.key === "Tab" && !event.shiftKey && focusedCard) {
        event.preventDefault();
        openTaskFromCard(focusedCard, true);
        return;
      }
      if ((event.key === "Home" || event.key === "End") && focusedCard) {
        event.preventDefault();
        const cards = taskCards();
        const card = event.key === "Home" ? cards[0] : cards[cards.length - 1];
        if (card) openTaskFromCard(card, false);
        return;
      }
      if (event.key === "F1" || (event.key === "?" && !isEditing(target))) {
        event.preventDefault();
        shortcutOpener = target;
        shortcutOpenerTaskId = target?.closest<HTMLElement>("[data-task-id]")?.dataset.taskId;
        props.onOpenChange(true);
        return;
      }
      if (isEditing(target)) return;
      if (event.key === "Delete" && event.shiftKey) {
        event.preventDefault();
        purgeSelectedTask();
        return;
      }
      if (event.shiftKey) return;

      const key = event.key.toLowerCase();
      if (key === "/") {
        event.preventDefault();
        const prompt = document.querySelector<HTMLElement>("[data-testid='task-detail-prompt']")
          ?? document.querySelector<HTMLElement>("[data-testid='prompt-input']");
        prompt?.focus();
      } else if (key === "r") {
        event.preventDefault();
        openNewTaskControl("[data-testid='add-repo-button']", true);
      } else if (key === "n") {
        event.preventDefault();
        openNewTaskControl("[data-testid='prompt-input']", false);
      } else if (key === "j" || event.key === "ArrowDown") {
        event.preventDefault();
        navigateTask(1, false);
      } else if (key === "k" || event.key === "ArrowUp") {
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
        <ShortcutSection title="Navigation" shortcuts={globalShortcuts} />
        <ShortcutSection title="While typing" shortcuts={promptShortcuts} />
        <ShortcutSection title="Repository picker" shortcuts={pickerShortcuts} />
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
