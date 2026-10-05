// API-only tests for repos and harnesses endpoints.
import { test, expect, waitForTaskState } from "../helpers";

test("Antigravity task selection survives creation and retrieval", async ({ api }) => {
  const repos = await api.listRepos();
  const created = await api.createTask({
    initialPrompt: { text: "Antigravity selection" },
    repos: [{ name: repos[0].path }],
    harness: "antigravity",
  });
  expect(created.harness).toBe("antigravity");
  const task = await waitForTaskState(api, created.id, "waiting");
  expect(task.harness).toBe("antigravity");
});

test("list repos returns the fake repo", async ({ api }) => {
  const repos = await api.listRepos();
  expect(repos.length).toBeGreaterThan(0);
  const repo = repos[0];
  expect(repo.path).toBeTruthy();
  expect(repo.baseBranch.name).toBe("main");
});

test("list harnesses returns quota groups for known fake harnesses", async ({ api }) => {
  const harnesses = await api.listHarnesses();
  expect(harnesses.length).toBeGreaterThan(0);
  const agy = harnesses.find((h) => h.name === "antigravity");
  expect(agy?.models.map((m) => m.id)).toContain("fake-model");
  expect(agy?.quotaGroup).toBeUndefined();
  expect(agy?.supportsImages).toBe(false);
  expect(agy?.supportsCompact).toBe(false);
  const claude = harnesses.find((h) => h.name === "claude");
  const codex = harnesses.find((h) => h.name === "codex");
  const pi = harnesses.find((h) => h.name === "pi");
  expect(claude).toBeTruthy();
  expect(claude!.models.map((m) => m.id)).toContain("fake-model");
  expect(claude!.quotaGroup).toBe("claudecode");
  expect(codex?.quotaGroup).toBe("codex");
  expect(pi?.quotaGroup).toBeUndefined();
});
