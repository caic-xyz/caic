// StatsPage is the /task/:taskId/stats route for task usage, timing, and resource analytics.

import { createMemo, Show } from "solid-js";
import { useParams } from "@solidjs/router";

import type { EventStats } from "@sdk/types.gen";

import { useAppState } from "../AppState";
import { DetailPane } from "../components/Layout";
import StatsDetail from "../components/StatsDetail";
import { createTaskEventTimeline } from "../taskEventTimeline";
import { taskIdFromParam, taskPathForTask } from "../taskPath";
import { IncrementalTaskTimingTracker } from "../timing";

function StatsPane(props: { taskId: string }) {
  const state = useAppState();
  const task = () => state.taskById(props.taskId);
  const timeline = createTaskEventTimeline({
    taskId: () => props.taskId,
    taskState: () => task()?.state ?? "pending",
    onError: state.showWarning,
  });
  const timingTracker = new IncrementalTaskTimingTracker();
  let timingEpoch = -1;
  const timings = createMemo(() => {
    const epoch = timeline.epoch();
    const reset = epoch !== timingEpoch;
    timingEpoch = epoch;
    return timingTracker.derive(timeline.messages(), reset);
  });
  const stats = createMemo<EventStats[]>(() =>
    timeline
      .messages()
      .filter((event) => event.kind === "stats" && event.stats !== undefined)
      .map((event) => event.stats as EventStats),
  );
  const taskPath = () => {
    const current = task();
    return current ? taskPathForTask(current) : `/task/@${props.taskId}`;
  };

  return (
    <StatsDetail
      taskPath={taskPath()}
      repo={task()?.repos?.[0]?.name ?? ""}
      branch={task()?.repos?.[0]?.branch ?? ""}
      events={timeline.messages()}
      stats={stats()}
      turns={timings().turns}
      usage={{
        inputTokens: task()?.cumulativeInputTokens ?? 0,
        cacheWriteInputTokens: task()?.cumulativeCacheCreationInputTokens ?? 0,
        cacheReadInputTokens: task()?.cumulativeCacheReadInputTokens ?? 0,
        outputTokens: task()?.cumulativeOutputTokens ?? 0,
        costUSD: task()?.costUSD ?? 0,
      }}
    />
  );
}

export default function StatsPage() {
  const params = useParams();
  const id = () => taskIdFromParam(params.taskId);

  return (
    <Show when={id()} keyed>
      {(taskId) => (
        <DetailPane>
          <StatsPane taskId={taskId} />
        </DetailPane>
      )}
    </Show>
  );
}
