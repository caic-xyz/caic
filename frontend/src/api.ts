// Singleton API client for the caic web UI.
import { createApiClient } from "@sdk/api.gen";
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
  listHarnesses,
  listCaches,
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
