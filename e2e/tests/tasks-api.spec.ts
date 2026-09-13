// API-only tests for task lifecycle and repository status endpoints (no browser UI).
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

test("create task and reach waiting state via API", async ({ api }) => {
  const id = await createTaskAPI(api, "api lifecycle test");

  // The fake agent completes one turn then the task enters waiting state.
  const task = await waitForTaskState(api, id, "waiting");
  expect(task.harness).toBe("claude");
  expect(task.numTurns).toBeGreaterThanOrEqual(1);
});

test("task diff reports repository git status", async ({ api }) => {
  const id = await createTaskAPI(api, "api git status test");
  const task = await waitForTaskState(api, id, "waiting");

  const diff = await api.getTaskDiff(id, "");
  expect(diff.diff).toBe("");
  expect(diff.repositories).toHaveLength(1);
  expect(diff.repositories[0]).toEqual({
    name: task.repos![0].name,
    branch: task.repos![0].branch,
    upstream: "origin/main",
    ahead: 1,
    behind: 0,
    commits: [{
      sha: "7b14c36e1f5a0d2c9e8f4b6a3c1d0e9f8a7b6c5d",
      subject: "Add task activity summary",
      authoredDate: "2026-09-01T10:30:00Z",
      stat: [{ path: "cmd/caic/main.go", added: 8, deleted: 0 }],
    }],
    uncommitted: [{
      path: "frontend/src/App.tsx",
      worktreeStatus: "M",
      added: 4,
      deleted: 2,
      binary: false,
      diff: "",
    }],
  });
});

test("task repository status reports compact git state", async ({ api }) => {
  const id = await createTaskAPI(api, "api compact git status test");
  const task = await waitForTaskState(api, id, "waiting");

  const status = await api.getTaskRepoStatus(id);
  expect(status).toEqual({
    repositories: [{
      name: task.repos![0].name,
      branch: expect.stringMatching(/^caic-\d+$/),
      ahead: 1,
      behind: 0,
      changedFiles: 2,
      added: 12,
      deleted: 2,
      uncommittedFiles: 1,
      conflicts: 0,
    }],
  });
});

test("purge a waiting task via API", async ({ api }) => {
  const id = await createTaskAPI(api, "api purge test");
  await waitForTaskState(api, id, "waiting");

  await api.purgeTask(id);
  await waitForTaskState(api, id, "purged");
});

test("send input to a waiting task triggers another turn", async ({ api }) => {
  const id = await createTaskAPI(api, "api input test");
  await waitForTaskState(api, id, "waiting");

  await api.sendInput(id, { prompt: { text: "continue" } });

  // After input the task runs again and returns to waiting.
  const task = await waitForTaskState(api, id, "waiting", 20_000);
  expect(task.numTurns).toBeGreaterThanOrEqual(2);
});

test("restart a waiting task starts a new session", async ({ api }) => {
  const id = await createTaskAPI(api, "api restart test");
  await waitForTaskState(api, id, "waiting");

  // Restart while waiting — this starts a new agent session with a new prompt.
  await api.restartTask(id, { prompt: { text: "try again" } });
  const task = await waitForTaskState(api, id, "waiting", 20_000);
  // numTurns may reset on restart; just verify the task completed another turn.
  expect(task.numTurns).toBeGreaterThanOrEqual(1);
});

test("fake backend sets PR and CI status that cycles to success", async ({ api }) => {
  const id = await createTaskAPI(api, "api ci dot test");
  await waitForTaskState(api, id, "waiting");

  // Fake backend sets PR #1, pending CI, and 3 check runs shortly after reaching waiting.
  await expect(async () => {
    const t = await api.getTask(id);
    expect(t!.forgePR).toBe(1);
    expect(t!.ciStatus).toBeDefined();
    expect(t!.ciChecks?.length).toBe(3);
  }).toPass({ timeout: 5_000, intervals: [500] });

  // CI transitions to success within ~5s (checks complete one per second).
  await expect(async () => {
    const t = await api.getTask(id);
    expect(t!.ciStatus).toBe("success");
  }).toPass({ timeout: 10_000, intervals: [500] });
});

test("list tasks includes created task", async ({ api }) => {
  const id = await createTaskAPI(api, "api list test");

  // Wait for the task to reach waiting so all fields are populated.
  await waitForTaskState(api, id, "waiting");

  const tasks = await api.listTasks();
  const found = tasks.find((t) => t.id === id);
  expect(found).toBeTruthy();
  expect(found!.initialPrompt).toBe("api list test");
  expect(found!.repos![0].name).toBeTruthy();
  expect(found!.repos![0].branch).toBeTruthy();
});
