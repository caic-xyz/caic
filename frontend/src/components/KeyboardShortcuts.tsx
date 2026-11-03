// Global keyboard navigation and the discoverable keyboard-shortcuts reference dialog.

import { For, onCleanup, onMount, Show } from "solid-js";
import { useLocation } from "@solidjs/router";
import { taskViewMode } from "../taskView";

import { useAppState } from "../AppState";
import { taskPathForTask } from "../taskPath";
import ModalDialog from "./ModalDialog";
import styles from "./KeyboardShortcuts.module.css";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  voiceAvailable: boolean;
}

const navigationShortcutsAfterModelSettings = [
  { keys: "/", action: "Focus the current prompt" },
  { keys: "↓ / Page Down", action: "Select the next task" },
  { keys: "↑ / Page Up", action: "Select the previous task" },
  { keys: "Tab", action: "Move from a task card to its prompt" },
  { keys: "Shift + Tab", action: "Return from a prompt to its task card" },
  { keys: "Esc", action: "Close a dialog, otherwise focus the new-task prompt" },
  { keys: "? / F1", action: "Show keyboard shortcuts" },
];

const promptShortcuts = [
  { keys: "Page Up / Page Down", action: "Switch tasks and keep typing" },
  { keys: "Esc", action: "Focus the new-task prompt" },
];

const taskActionShortcuts = [
  { keys: "Shift + Enter", action: "Insert a newline in the prompt" },
  { keys: "Ctrl + Enter", action: "Steer the task immediately, or submit a new task" },
  { keys: "Shift + Delete", action: "Purge the selected task" },
];

function isEditing(target: EventTarget | null): boolean {
  return (
    target instanceof HTMLElement &&
    target.closest("input, textarea, select, [contenteditable='true'], [role='textbox']") !== null
  );
}

export default function KeyboardShortcuts(props: Props) {
  const location = useLocation();
  const s = useAppState();
  let shortcutOpener: HTMLElement | null = null;
  let shortcutOpenerTaskId: string | undefined;

  const navigationShortcuts = () => {
    const harnesses = s.harnesses();
    const f3 =
      harnesses.length > 1
        ? { keys: "F3", action: "Focus harness for the new task" }
        : harnesses.find((harness) => harness.name === s.selectedHarness())?.models.length
          ? { keys: "F3", action: "Focus model for the new task" }
          : s.runtimes().length > 1
            ? { keys: "F3", action: "Focus runtime for the new task" }
            : null;
    return [
      { keys: "F2", action: "Focus repositories for the new task" },
      ...(f3 ? [f3] : []),
      ...(props.voiceAvailable ? [{ keys: "F4", action: "Toggle voice mode" }] : []),
      ...navigationShortcutsAfterModelSettings,
    ];
  };

  function restoreShortcutFocus() {
    if (shortcutOpener?.isConnected) {
      shortcutOpener.focus();
      return;
    }
    taskCards()
      .find((card) => card.dataset.taskId === shortcutOpenerTaskId)
      ?.focus();
  }

  function focusNewTaskControl(selector: string) {
    s.navigate("/");
    requestAnimationFrame(() => {
      const el = document.querySelector<HTMLElement>(selector);
      if (el?.matches("[data-testid='runtime-select']")) s.setFeaturesOpen(true);
      el?.focus();
    });
  }

  function focusCurrentPrompt() {
    const taskId = s.selectedId();
    if (!taskId) {
      document.querySelector<HTMLElement>("[data-testid='prompt-input']")?.focus();
      return;
    }

    const prompt = document.querySelector<HTMLElement>("[data-testid='task-detail-prompt']");
    if (prompt) {
      prompt.focus();
      return;
    }

    const task = s.selectedTask();
    s.navigate(task ? taskPathForTask(task) : `/task/@${taskId}`);
    requestAnimationFrame(() => document.querySelector<HTMLElement>("[data-testid='task-detail-prompt']")?.focus());
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
      const currentCard = Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]")).find(
        (el) => el.dataset.taskId === task.id,
      );
      currentCard?.focus();
    });
  }

  const taskCards = () => Array.from(document.querySelectorAll<HTMLElement>("[data-task-id]"));

  // navigateTask reports whether a task list was there to move through.
  function navigateTask(
    delta: number,
    focusPrompt: boolean,
    currentTaskId: string | null | undefined = s.selectedId(),
  ): boolean {
    const cards = taskCards();
    if (cards.length === 0) return false;
    const current = cards.findIndex((el) => el.dataset.taskId === currentTaskId);
    const next = current === -1 ? (delta > 0 ? 0 : cards.length - 1) : (current + delta + cards.length) % cards.length;
    const card = cards[next];
    if (!card) return false;
    openTaskFromCard(card, focusPrompt);
    return true;
  }

  function purgeSelectedTask() {
    const task = s.selectedTask();
    if (!task || new Set(["failed", "purged", "purging", "stopping"]).has(task.state)) return;
    s.handlePurge(task.id);
  }

  onMount(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey) return;
      if (document.querySelector("dialog[open]")) return;

      const target = event.target instanceof HTMLElement ? event.target : null;
      const taskPrompt = target?.closest("[data-testid='task-detail-prompt'], [data-testid='prompt-input']");
      const focusedCard = target?.matches("[data-task-id]") ? target : null;
      // Subview keys stay within the active pane instead of navigating the task list.
      if (
        taskViewMode(location.pathname, location.search) !== "closed" &&
        !target?.closest("[data-testid='conversation-pane']") &&
        !focusedCard
      )
        return;
      if (event.key === "/" && (!isEditing(target) || target?.matches("input[type='checkbox']"))) {
        event.preventDefault();
        focusCurrentPrompt();
        return;
      }
      if (event.key === "Escape") {
        if (taskViewMode(location.pathname, location.search) !== "closed") return;
        event.preventDefault();
        focusNewTaskControl("[data-testid='prompt-input']");
        return;
      }
      if (
        focusedCard &&
        !event.shiftKey &&
        (event.key === "ArrowDown" || event.key === "ArrowUp" || event.key === "PageDown" || event.key === "PageUp")
      ) {
        event.preventDefault();
        navigateTask(event.key === "ArrowDown" || event.key === "PageDown" ? 1 : -1, false, focusedCard.dataset.taskId);
        return;
      }
      if (
        !event.shiftKey &&
        (taskPrompt || !isEditing(target)) &&
        (event.key === "PageDown" || event.key === "PageUp")
      ) {
        if (!navigateTask(event.key === "PageDown" ? 1 : -1, true)) {
          // The shell scrolls a content column, not the document, so the
          // browser's own paging moves nothing: page that column here.
          const scroller = pageScroller(target);
          if (!scroller) return;
          event.preventDefault();
          const step = Math.max(80, scroller.clientHeight - 40);
          scroller.scrollBy({ top: event.key === "PageDown" ? step : -step, behavior: "auto" });
        } else {
          event.preventDefault();
        }
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
        focusNewTaskControl(
          "[data-testid='harness-select'], [data-testid='model-select'], [data-testid='effort-select'], [data-testid='runtime-select']",
        );
        return;
      }
      if (event.key === "Delete" && event.shiftKey) {
        event.preventDefault();
        purgeSelectedTask();
        return;
      }
      if (isEditing(target)) return;
      if (event.shiftKey) return;

      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        // Without a task list the arrows keep their scrolling behaviour.
        if (!navigateTask(event.key === "ArrowDown" ? 1 : -1, false)) return;
        event.preventDefault();
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
        <p class={styles.intro}>
          Move between task cards and prompts without leaving the keyboard. Local menus and dialogs handle Escape first.
        </p>
        <ShortcutSection title="Navigation" shortcuts={navigationShortcuts()} />
        <ShortcutSection title="While typing" shortcuts={promptShortcuts} />
        <ShortcutSection title="Task actions" shortcuts={taskActionShortcuts} />
        <button type="button" class={styles.closeButton} onClick={() => props.onOpenChange(false)}>
          Close
        </button>
      </ModalDialog>
    </Show>
  );
}

// pageScroller returns the column a page key scrolls: the focused element's
// own scroller, or the shell's content column when nothing is focused.
function pageScroller(target: HTMLElement | null): HTMLElement | null {
  for (let el: HTMLElement | null = target; el; el = el.parentElement) {
    if (el.scrollHeight > el.clientHeight && /^(auto|scroll)$/.test(getComputedStyle(el).overflowY)) return el;
  }
  const main = document.querySelector<HTMLElement>("main");
  if (main && main.scrollHeight > main.clientHeight) return main;
  return null;
}

function ShortcutSection(props: { title: string; shortcuts: { keys: string; action: string }[] }) {
  return (
    <section class={styles.section}>
      <h3 class={styles.sectionTitle}>{props.title}</h3>
      <dl class={styles.shortcutList}>
        <For each={props.shortcuts}>
          {(shortcut) => (
            <div class={styles.shortcutRow}>
              <dt class={styles.keys}>
                <kbd>{shortcut.keys}</kbd>
              </dt>
              <dd class={styles.action}>{shortcut.action}</dd>
            </div>
          )}
        </For>
      </dl>
    </section>
  );
}
