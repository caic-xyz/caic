// API-only tests for repos and harnesses endpoints.
import { test, expect } from "../helpers";

test("list repos returns the fake repo", async ({ api }) => {
  const repos = await api.listRepos();
  expect(repos.length).toBeGreaterThan(0);
  const repo = repos[0];
  expect(repo.path).toBeTruthy();
  expect(repo.baseBranch.name).toBe("main");
});

test("list harnesses returns the Claude-compatible fake harness", async ({ api }) => {
  const harnesses = await api.listHarnesses();
  expect(harnesses.length).toBeGreaterThan(0);
  const claude = harnesses.find((h) => h.name === "claude");
  expect(claude).toBeTruthy();
  expect(claude!.models.map((m) => m.id)).toContain("fake-model");
});
