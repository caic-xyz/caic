// API-only tests for repos and harnesses endpoints.
import { test, expect } from "../helpers";

test("list repos returns the fake repo", async ({ api }) => {
  const repos = await api.listRepos();
  expect(repos.length).toBeGreaterThan(0);
  const repo = repos[0];
  expect(repo.path).toBeTruthy();
  expect(repo.baseBranch.name).toBe("main");
});

test("list harnesses returns the fake harness", async ({ api }) => {
  const harnesses = await api.listHarnesses();
  expect(harnesses.length).toBeGreaterThan(0);
  const fake = harnesses.find((h) => h.name === "fake");
  expect(fake).toBeTruthy();
  expect(fake!.models).toContain("fake-model");
});
