// StatsPage displays statistics derived from shared task history and lazy cost estimates.

import { Show } from "solid-js";
import { useParams } from "@solidjs/router";

import { useAppState } from "../AppState";
import { DetailPane } from "../components/Layout";
import StatsDetail from "../components/StatsDetail";
import { useTaskTimeline } from "../TaskTimelineContext";
import { createTaskAnalytics } from "../createTaskAnalytics";
import Button from "../components/Button";
import { taskIdFromParam, taskPathForTask } from "../taskPath";

function StatsPane(props: { taskId: string }) {
  const state = useAppState();
  const task = () => state.taskById(props.taskId);
  const analytics = createTaskAnalytics(useTaskTimeline());
  const taskPath = () => {
    const current = task();
    return current ? taskPathForTask(current) : `/task/@${props.taskId}`;
  };

  return (
    <StatsDetail
      status={
        analytics.loading() || analytics.error() || analytics.pricing() || !analytics.snapshot().costComplete ? (
          <>
            <Show when={analytics.loading()}>
              <p role="status">Loading statistics…</p>
            </Show>
            <Show when={analytics.error()}>
              {(message) => (
                <p role="alert">
                  Statistics incomplete: {message()} <Button onClick={analytics.retry}>Retry statistics</Button>
                </p>
              )}
            </Show>
            <Show when={!analytics.loading() && analytics.pricing()}>
              <p role="status">Updating cost estimates…</p>
            </Show>
            <Show when={!analytics.loading() && !analytics.pricing() && !analytics.snapshot().costComplete}>
              <p role="status">Some turn costs are unavailable.</p>
            </Show>
          </>
        ) : null
      }
      taskPath={taskPath()}
      repo={task()?.repos?.[0]?.name ?? ""}
      branch={task()?.repos?.[0]?.branch ?? ""}
      tools={analytics.snapshot().tools}
      stats={analytics.stats()}
      turns={analytics.snapshot().turns}
      usage={{
        inputTokens: task()?.cumulativeInputTokens ?? 0,
        cacheWriteInputTokens: task()?.cumulativeCacheCreationInputTokens ?? 0,
        cacheReadInputTokens: task()?.cumulativeCacheReadInputTokens ?? 0,
        outputTokens: task()?.cumulativeOutputTokens ?? 0,
        costUSD: task()?.costUSD ?? (analytics.snapshot().costComplete ? analytics.snapshot().costUSD : 0),
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
