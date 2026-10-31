// Browser API clients attach CSRF protection, submit prompt files atomically, and bind logout to its session.

import { createApiClient, type FetchFn } from "@sdk/api.gen";
import type { CreateTaskReq, ForkTaskReq, InputReq, Prompt, RestartReq } from "@sdk/types.gen";

export const csrfFetch: typeof fetch = (input, init) => {
  const headers = new Headers(init?.headers);
  headers.set("X-Caic-CSRF", "1");
  return fetch(input, { ...init, headers });
};

export interface PromptOptions {
  images: readonly Blob[];
  signal: AbortSignal;
}

function promptImages(prompt: Prompt, options: PromptOptions | undefined): Prompt {
  return options?.images.length
    ? { ...prompt, images: options.images.map((_, i) => ({ url: `cid:image-${i}` })) }
    : prompt;
}

function promptClient(fetchFn: FetchFn, options: PromptOptions | undefined) {
  return createApiClient(async (input, init) => {
    if (!options) return fetchFn(input, init);
    options.signal.throwIfAborted();
    const headers = new Headers(init?.headers);
    let body = init?.body;
    if (options.images.length) {
      const form = new FormData();
      form.append("request", String(body));
      options.images.forEach((image, i) => form.append(`image-${i}`, image));
      body = form;
      headers.delete("Content-Type");
    }
    const signal = init?.signal ? AbortSignal.any([init.signal, options.signal]) : options.signal;
    return fetchFn(input, { ...init, body, headers, signal });
  });
}

/** Adds atomic multipart image submissions to a generated JSON API client. */
export function createPromptClient(fetchFn: FetchFn) {
  return {
    ...createApiClient(fetchFn),
    createTask: (req: CreateTaskReq, options?: PromptOptions) =>
      promptClient(fetchFn, options).createTask({ ...req, initialPrompt: promptImages(req.initialPrompt, options) }),
    sendInput: (id: string, req: InputReq, options?: PromptOptions) =>
      promptClient(fetchFn, options).sendInput(id, { ...req, prompt: promptImages(req.prompt, options) }),
    restartTask: (id: string, req: RestartReq, options?: PromptOptions) =>
      promptClient(fetchFn, options).restartTask(id, { ...req, prompt: promptImages(req.prompt, options) }),
    forkTask: (id: string, req: ForkTaskReq, options?: PromptOptions) =>
      promptClient(fetchFn, options).forkTask(id, { ...req, prompt: promptImages(req.prompt, options) }),
  };
}

export const api = {
  ...createPromptClient(csrfFetch),
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
