// Shares current-context history, server summaries, and disclosure reads across selected-task subviews.
import { createMemo, createSignal, untrack, type Accessor } from "solid-js";
import type {
  EventMessage,
  EventStats,
  HistoryActivity,
  HistoryEvent,
  HistoryManifest,
  HistoryPosition,
  HistorySection,
  HistoryTurnEstimate,
} from "@sdk/types.gen";
import { createTaskHistory } from "./taskHistory";

export interface TaskHistory {
  // Read-only owner view: live publications append in place; context replacement changes epoch.
  messages: Accessor<readonly EventMessage[]>;
  manifest: Accessor<HistoryManifest | null>;
  stats: Accessor<EventStats[]>;
  through: Accessor<number>;
  epoch: Accessor<number>;
  loading: Accessor<boolean>;
  error: Accessor<string | null>;
  retry: () => void;
  position: (message: EventMessage) => HistoryPosition | null;
  readSection: (section: HistorySection, signal: AbortSignal) => Promise<HistoryEvent[]>;
  readSetup: (signal: AbortSignal) => Promise<HistoryEvent[]>;
  readActivity: (
    activity: HistoryActivity,
    through: number,
    after: HistoryPosition | null,
    signal: AbortSignal,
  ) => Promise<HistoryEvent[]>;
  readCosts: (signal: AbortSignal) => Promise<HistoryTurnEstimate[]>;
}

export function createTaskEventTimeline(options: { taskId: Accessor<string> }): TaskHistory {
  const [attempt, setAttempt] = createSignal(0);
  const source = createMemo(() => {
    attempt();
    const id = options.taskId();
    return untrack(() => createTaskHistory(id));
  });
  let revision = 0;
  const epoch = createMemo(() => {
    source().epoch();
    return ++revision;
  });
  return {
    messages: () => source().messages(),
    manifest: () => source().manifest(),
    stats: () => source().stats(),
    through: () => source().through(),
    epoch,
    loading: () => source().loading(),
    error: () => source().error(),
    retry: () => setAttempt((previous) => previous + 1),
    position: (message) => source().position(message),
    readSection: (section, signal) => source().readSection(section, signal),
    readSetup: (signal) => source().readSetup(signal),
    readActivity: (activity, through, after, signal) => source().readActivity(activity, through, after, signal),
    readCosts: (signal) => source().readCosts(signal),
  };
}
