// Singleton API client for the caic web UI.
import { createApiClient } from "@sdk/api.gen";

export const api = createApiClient();

export const {
  getConfig,
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
  taskFixPR,
  restartTask,
  stopTask,
  purgeTask,
  reviveTask,
  getTaskCILog,
  syncTask,
  getTaskDiff,
  getTaskToolInput,
  globalTaskEvents,
  globalUsageEvents,
  getUsage,
  getVoiceToken,
  voiceRTCOffer,
  webFetch,
} = api;
