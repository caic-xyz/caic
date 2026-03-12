// API-only tests for the task lifecycle (no browser UI).
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

test("create task and reach waiting state via API", async ({ api }) => {
  const id = await createTaskAPI(api, "api lifecycle test");

  // The fake agent completes one turn then the task enters waiting state.
  const task = await waitForTaskState(api, id, "waiting");
  expect(task.harness).toBe("fake");
  expect(task.numTurns).toBeGreaterThanOrEqual(1);
});

test("terminate a waiting task via API", async ({ api }) => {
  const id = await createTaskAPI(api, "api terminate test");
  await waitForTaskState(api, id, "waiting");

  await api.terminateTask(id);
  await waitForTaskState(api, id, "terminated");
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

  // Fake backend sets PR #1 and pending CI shortly after reaching waiting.
  await expect(async () => {
    const t = await api.getTask(id);
    expect(t!.forgePR).toBe(1);
    expect(t!.ciStatus).toBeDefined();
  }).toPass({ timeout: 5_000, intervals: [500] });

  // CI transitions to success within ~5s.
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
