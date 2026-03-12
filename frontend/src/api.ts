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
  listRepos,
  cloneRepo,
  listRepoBranches,
  listTasks,
  createTask,
  taskRawEvents,
  taskEvents,
  sendInput,
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
  webFetch,
} = api;
