// Exercises the generated SDK across concurrent task creation, sync, input, restart, and purge.
import { ok } from "node:assert/strict";
import { test, expect, createTaskAPI, waitForTaskState, recentHistoryPage } from "../helpers";

test("SDK task lifecycle keeps concurrent tasks isolated", async ({ api, uniquePrompt }) => {
  const prompt = uniquePrompt("SDK lifecycle A");
  const [id, otherID] = await Promise.all([
    createTaskAPI(api, prompt),
    createTaskAPI(api, uniquePrompt("SDK lifecycle B")),
  ]);
  const [initial, other] = await Promise.all([
    waitForTaskState(api, id, "waiting"),
    waitForTaskState(api, otherID, "waiting"),
  ]);
  const repo = initial.repos?.[0];
  ok(repo);
  expect(initial.harness).toBe("claude");
  expect(initial.numTurns).toBe(1);
  expect(repo.branch).not.toBe(other.repos?.[0]?.branch);

  await test.step("list and sync preserve task and branch identity", async () => {
    expect(await api.listTasks()).toContainEqual(expect.objectContaining({ id, initialPrompt: prompt }));
    for (const [target, branch] of [
      ["default", "main"],
      ["branch", repo.branch],
    ] as const) {
      const result = await api.syncTask(id, { target });
      expect(result.status).toBe("empty");
      expect(result.branch).toBe(branch);
      expect(result.safetyIssues ?? []).toEqual([]);
    }
  });

  await test.step("input continues the session and restart starts a fresh turn", async () => {
    await api.sendInput(id, { prompt: { text: "tell me another" } });
    const continued = await waitForTaskState(api, id, "waiting");
    expect(continued.numTurns).toBeGreaterThan(initial.numTurns);

    await api.restartTask(id, { prompt: { text: "start over" } });
    const restarted = await waitForTaskState(api, id, "waiting");
    const bootstrap = (await api.getHistoryManifest(id)).bootstrap;
    const recent = recentHistoryPage(bootstrap);
    const events = await api.getHistorySlice(
      id,
      bootstrap.scope,
      bootstrap.projectionVersion,
      bootstrap.generation,
      String(recent.first),
      "0",
      String(recent.last),
      "end",
    );
    // Reaching waiting alone could pass without a restart. The first joke
    // proves that the agent's session-local turn counter started over.
    const result = events.filter((event) => event.message.kind === "result").at(-1);
    expect(result?.message.result?.result).toContain("Why do programmers prefer dark mode?");
    expect(restarted.repos?.[0]?.branch).toBe(repo.branch);
    expect(await api.getTask(otherID)).toMatchObject({
      state: "waiting",
      numTurns: other.numTurns,
      repos: other.repos,
    });
  });

  await test.step("purge both tasks", async () => {
    await Promise.all([api.purgeTask(id), api.purgeTask(otherID)]);
    await Promise.all([waitForTaskState(api, id, "purged"), waitForTaskState(api, otherID, "purged")]);
  });
});
