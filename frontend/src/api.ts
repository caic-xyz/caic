// Browser API clients attach CSRF protection and bind logout to its session.

import { createApiClient } from "@sdk/api.gen";

const csrfFetch: typeof fetch = (input, init) => {
  const headers = new Headers(init?.headers);
  headers.set("X-Caic-CSRF", "1");
  return fetch(input, { ...init, headers });
};

export const api = {
  ...createApiClient(csrfFetch),
  logout: (sessionID: string) =>
    createApiClient((input, init) => {
      const headers = new Headers(init?.headers);
      headers.set("X-Caic-Session", sessionID);
      return csrfFetch(input, { ...init, headers });
    }).logout(),
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
