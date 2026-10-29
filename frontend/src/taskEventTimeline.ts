// Owns cached task history and resource samples for the selected task.
import { createMemo, createSignal, untrack, type Accessor } from "solid-js";
import type { EventMessage, EventStats } from "@sdk/types.gen";
import { createTaskHistory } from "./taskHistory";
import { type PreviewText } from "./recentTextPreview";
import type { ResultEstimate } from "./taskAnalytics";

export interface TaskHistory {
  messages: Accessor<EventMessage[]>;
  preview: Accessor<PreviewText | null>;
  stats: Accessor<EventStats[]>;
  // Replacements change epoch; messages within an epoch only append.
  epoch: Accessor<number>;
  loading: Accessor<boolean>;
  error: Accessor<string | null>;
  retry: () => void;
  // Prices refer to source-owned root result objects, never public raw positions.
  readCosts: (signal: AbortSignal) => Promise<readonly ResultEstimate[]>;
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
    preview: () => source().preview(),
    stats: () => source().stats(),
    epoch,
    loading: () => source().loading(),
    error: () => source().error(),
    retry: () => setAttempt((previous) => previous + 1),
    readCosts: (signal) => source().readCosts(signal),
  };
}
