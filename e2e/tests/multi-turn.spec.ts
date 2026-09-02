// Multi-turn interaction and concurrent task tests.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

test("multi-turn: send input cycles to next joke", async ({ page, api }) => {
  const id = await createTaskAPI(api, "multi-turn test");
  await waitForTaskState(api, id, "waiting");

  // Open the task in the browser to verify streaming output.
  await page.goto("/");
  const taskCard = page.locator(`[data-task-id="${id}"]`);
  await expect(taskCard).toBeVisible({ timeout: 10_000 });
  await taskCard.click();

  // First joke should be visible.
  await expect(
    page.getByText("Why do programmers prefer dark mode?").first(),
  ).toBeVisible({ timeout: 15_000 });

  // Send input after a measurable user wait to trigger the second turn.
  await page.waitForTimeout(1100);
  await api.sendInput(id, { prompt: { text: "tell me another" } });
  await waitForTaskState(api, id, "waiting", 20_000);

  // Second joke: "A SQL query walks into a bar..."
  await expect(
    page.getByText("A SQL query walks into a bar").first(),
  ).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole("button", { name: /1 message.*0:01$/ })).toBeVisible();
  await expect(page.getByText(/^wait (?:[1-9]\d*ms|\d+:[0-5]\d(?::[0-5]\d)?)$/)).toBeVisible();
});

test("fake timing reaches tool rows and collapsed turn summaries", async ({ page, api }) => {
  const id = await createTaskAPI(api, "FAKE_DEMO timing propagation");
  await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto(`/task/@${id}`);
  await expect(page.getByTestId("tool-duration").filter({ hasText: /^180ms$/ })).toHaveCount(1);
  await expect(page.getByTestId("tool-duration").filter({ hasText: /^0:01$/ })).toHaveCount(1);
  await expect(page.getByTestId("timing-duration").filter({ hasText: /^0:02$/ })).toBeVisible();

  await page.waitForTimeout(1100);
  await api.sendInput(id, { prompt: { text: "continue" } });
  await waitForTaskState(api, id, "waiting", 20_000);

  await expect(page.getByRole("button", { name: /2 messages, 4 tool calls.*0:02$/ })).toBeVisible();
  await expect(page.getByText(/^wait (?:[1-9]\d*ms|\d+:[0-5]\d(?::[0-5]\d)?)$/)).toBeVisible();
});

test("concurrent tasks run independently", async ({ api }) => {
  const id1 = await createTaskAPI(api, "concurrent task A");
  const id2 = await createTaskAPI(api, "concurrent task B");

  // Both should reach waiting state independently.
  const [task1, task2] = await Promise.all([
    waitForTaskState(api, id1, "waiting"),
    waitForTaskState(api, id2, "waiting"),
  ]);

  expect(task1.id).not.toBe(task2.id);
  expect(task1.repos![0].branch).not.toBe(task2.repos![0].branch);

  // Purge both.
  await Promise.all([
    api.purgeTask(id1),
    api.purgeTask(id2),
  ]);
  await Promise.all([
    waitForTaskState(api, id1, "purged"),
    waitForTaskState(api, id2, "purged"),
  ]);
});

test("SSE event stream delivers text deltas", async ({ page, api, baseURL }) => {
  const id = await createTaskAPI(api, "sse stream test");

  // Wait for the first turn to finish; the agent is now paused waiting for input.
  await waitForTaskState(api, id, "waiting");

  // Navigate to the app so the browser has the right origin.
  await page.goto("/");

  // Expose a callback so the browser can signal when SSE history replay ends.
  let resolveReady!: () => void;
  const sseReady = new Promise<void>((res) => { resolveReady = res; });
  await page.exposeFunction("__sseReady", () => resolveReady());

  // Collect only live events (after the server "ready" sentinel).
  const eventsPromise = page.evaluate(async ({ taskId, base }) => {
    return new Promise<string[]>((resolve) => {
      const collected: string[] = [];
      let live = false;
      const es = new EventSource(`${base}/api/caic/v1/tasks/${taskId}/events`);
      es.addEventListener("ready", () => {
        live = true;
        (window as unknown as { __sseReady: () => void }).__sseReady();
      });
      es.addEventListener("message", (e) => {
        const msg = JSON.parse(e.data);
        if (live) {
          collected.push(msg.kind);
          if (msg.kind === "result") {
            es.close();
            resolve(collected);
          }
        }
      });
      // Safety timeout.
      setTimeout(() => { es.close(); resolve(collected); }, 15_000);
    });
  }, { taskId: id, base: baseURL! });

  // Wait until SSE history replay is done and the stream is live.
  await sseReady;

  // Trigger a second agent turn — live textDelta events should follow.
  await api.sendInput(id, { prompt: { text: "tell me another" } });

  const events = await eventsPromise;

  // The live stream from the second turn should include text deltas.
  expect(events).toContain("textDelta");
  expect(events).toContain("text");
  expect(events).toContain("result");
});
