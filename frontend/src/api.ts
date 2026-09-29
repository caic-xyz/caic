// Singleton API client for the caic web UI.

import { createApiClient, type TaskEventsHandlers } from "@sdk/api.gen";
import { validateEventMessage, validateTaskEventBackward, validateTaskHistoryStreamError } from "@sdk/validate.gen";

function taskEventStreamWithQuery(id: string, query: URLSearchParams, handlers: TaskEventsHandlers): EventSource {
  const source = new EventSource(`/api/caic/v1/tasks/${encodeURIComponent(id)}/events?${query}`);
  source.addEventListener("message", (event) => {
    try {
      handlers.onMessage(validateEventMessage(JSON.parse(event.data)));
    } catch (error) {
      handlers.onError(error);
    }
  });
  source.addEventListener("backward", (event) => {
    if (!(event instanceof MessageEvent) || typeof event.data !== "string") return;
    try {
      handlers.onBackward?.(validateTaskEventBackward(JSON.parse(event.data)));
    } catch (error) {
      handlers.onError(error);
    }
  });
  if (handlers.onReady) source.addEventListener("ready", handlers.onReady);
  if (handlers.onReset) source.addEventListener("reset", handlers.onReset);
  source.addEventListener("error", (event) => {
    if (!(event instanceof MessageEvent) || typeof event.data !== "string") return;
    try {
      handlers.onHistoryError?.(validateTaskHistoryStreamError(JSON.parse(event.data)));
    } catch (error) {
      handlers.onError(error);
    }
  });
  return source;
}

export const api = {
  ...createApiClient(),
  taskEventBackfill: (id: string, handlers: TaskEventsHandlers) =>
    taskEventStreamWithQuery(id, new URLSearchParams({ backward: "1" }), handlers),
  // EventSource cannot set Last-Event-ID initially. Bootstrap through the URL;
  // native reconnects then send newer IDs in Last-Event-ID automatically.
  taskEventStreamFromLastEventID: (id: string, eventId: string, handlers: TaskEventsHandlers) =>
    taskEventStreamWithQuery(id, new URLSearchParams({ "last-event-id": eventId }), handlers),
};

export const {
  getConfig,
  getVersion,
  triggerUpdate,
  getMe,
  logout,
  getPreferences,
  updatePreferences,
  listOAuthGrants,
  revokeOAuthGrant,
  listHarnesses,
  refreshHarness,
  listCaches,
  getCacheSizes,
  listRepos,
  cloneRepo,
  listRepoBranches,
  botFixCI,
  botFixPR,
  listTasks,
  createTask,
  taskEvents: taskEventStream,
  sendInput,
  restartTask,
  clearContext,
  compactContext,
  getTaskHandoff,
  forkTask,
  stopTask,
  purgeTask,
  reviveTask,
  getTask,
  getTaskInfo,
  getTaskCILog,
  syncTask,
  getTaskDiff,
  getTaskDiffIndex,
  getTaskFileDiff,
  getTaskRepoStatus,
  getTaskProcesses,
  signalProcess,
  getTaskToolInput,
  globalTaskEvents,
  globalUsageEvents,
  getUsage,
  getUsageDashboard,
  webFetch,
} = api;
