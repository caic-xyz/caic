// Singleton API client for the caic web UI.

import { createApiClient } from "@sdk/api.gen";
import type { EventMessage } from "@sdk/types.gen";
import { validateEventMessage } from "@sdk/validate.gen";

export const api = createApiClient();

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
  listCaches,
  getCacheSizes,
  listRepos,
  cloneRepo,
  listRepoBranches,
  botFixCI,
  botFixPR,
  listTasks,
  createTask,
  taskRawEvents,
  taskEvents,
  sendInput,
  restartTask,
  clearContext,
  compactContext,
  forkTask,
  stopTask,
  purgeTask,
  reviveTask,
  getTask,
  getTaskInfo,
  getTaskCILog,
  syncTask,
  getTaskDiff,
  getTaskProcesses,
  signalProcess,
  getTaskToolInput,
  globalTaskEvents,
  globalUsageEvents,
  getUsage,
  webFetch,
} = api;

export interface TaskHistoryStreamError {
  message: string;
}

function validateTaskHistoryStreamError(value: unknown): TaskHistoryStreamError {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error("invalid task history error payload");
  }
  const message = (value as Record<string, unknown>).message;
  if (typeof message !== "string" || message.length === 0) {
    throw new Error("invalid task history error message");
  }
  return { message };
}

export function taskEventStream(
  id: string,
  onMessage: (event: EventMessage) => void,
  onError: (err: unknown) => void,
  onReady?: () => void,
  onOpen?: () => void,
  onHistoryError?: (error: TaskHistoryStreamError) => void,
  onReset?: () => void,
): EventSource {
  const es = new EventSource(`/api/caic/v1/tasks/${id}/events`);
  es.addEventListener("message", (e) => {
    const ev = e as MessageEvent<string>;
    try {
      onMessage(validateEventMessage(JSON.parse(ev.data)));
    } catch (err) {
      onError(err);
    }
  });
  es.addEventListener("error", (event) => {
    // Native EventSource connection failures are plain Events without data.
    // Only the server's named SSE error event carries a JSON history payload.
    if (!(event instanceof MessageEvent) || typeof event.data !== "string") return;
    try {
      onHistoryError?.(validateTaskHistoryStreamError(JSON.parse(event.data)));
    } catch (err) {
      onError(err);
    }
  });
  if (onReady) {
    es.addEventListener("ready", onReady);
  }
  if (onReset) {
    es.addEventListener("reset", onReset);
  }
  if (onOpen) {
    es.addEventListener("open", onOpen);
  }
  return es;
}
