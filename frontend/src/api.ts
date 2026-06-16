// Singleton API client for the caic web UI.
import { createApiClient } from "@sdk/api.gen";
import type { EventMessage } from "@sdk/types.gen";
import { validateEventMessage } from "@sdk/validate.gen";
import * as voicegatewaySDK from "@voicegateway-sdk/api.gen";

export const api = createApiClient();
export const voiceGatewayApi = voicegatewaySDK.createApiClient();

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

export const {
  voiceRTCOffer,
  closeVoiceRTC,
} = voiceGatewayApi;

export function taskEventStream(
  id: string,
  onMessage: (event: EventMessage) => void,
  onError: (err: unknown) => void,
  onReady?: () => void,
  onOpen?: () => void,
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
  if (onReady) {
    es.addEventListener("ready", onReady);
  }
  if (onOpen) {
    es.addEventListener("open", onOpen);
  }
  return es;
}
