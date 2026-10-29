// Browser coverage for concurrent owned tools, bounded output, independent failures, SSE recovery, and history totals.
import { createServer, request as httpRequest, type ServerResponse } from "node:http";
import { once } from "node:events";

import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`concurrent messages remain readable and stable at ${width}px`, async ({
    page,
    api,
    uniquePrompt,
  }, testInfo) => {
    const appErrors: string[] = [];
    page.on("pageerror", (error) => appErrors.push(error.message));
    await page.setViewportSize({ width, height: 900 });
    const id = await createTaskAPI(api, uniquePrompt("FAKE_CONCURRENT_STREAMS"));
    // Only this task's SSE crosses the proxy. Piping preserves backpressure;
    // destroying its response closes the real transport without replacing data.
    const connections = new Set<ServerResponse>();
    const cursors: string[] = [];
    let reconnectGate: ReturnType<typeof Promise.withResolvers<void>> | null = null;
    const proxy = createServer(async (request, response) => {
      cursors.push(String(request.headers["last-event-id"] ?? ""));
      const gate = reconnectGate;
      if (gate !== null) await gate.promise;
      if (response.destroyed) return;
      const upstream = httpRequest(
        `${testInfo.project.use.baseURL}${request.url}`,
        {
          headers: request.headers,
        },
        (stream) => {
          response.writeHead(stream.statusCode ?? 502, {
            ...stream.headers,
            "access-control-allow-origin": String(testInfo.project.use.baseURL),
            "access-control-allow-credentials": "true",
          });
          stream.on("error", (error) => response.destroy(error));
          stream.pipe(response);
        },
      );
      connections.add(response);
      response.on("close", () => {
        connections.delete(response);
        upstream.destroy();
      });
      upstream.on("error", (error) => response.destroy(error));
      upstream.end();
    });
    proxy.listen(0, "127.0.0.1");
    await once(proxy, "listening");
    const address = proxy.address();
    if (address === null || typeof address === "string") throw new Error("Missing proxy port");
    await page.route(`**/tasks/${id}/history/events?*`, (route) => {
      const url = new URL(route.request().url());
      url.port = String(address.port);
      return route.continue({ url: url.toString() });
    });
    try {
      await page.goto(`/task/@${id}`);

      const cards = page.getByTestId("native-subagent-card");
      const cardA = page.locator('[data-native-id="codex:thread:child-a"]');
      const cardB = page.locator('[data-native-id="codex:thread:child-b"]');
      const tool = (thread: string) =>
        page.locator(`[data-tool-key='${JSON.stringify([thread, "", "review-turn", "", "shared-tool"])}']`);
      const toolA = tool("child-a");
      const toolB = tool("child-b");
      const rootTool = tool("parent");
      const groupA = cardA.locator("details").filter({ has: toolA }).first();
      const groupB = cardB.locator("details").filter({ has: toolB }).first();
      const totals = async () => {
        const task = await api.getTask(id);
        return {
          input: task.cumulativeInputTokens,
          output: task.cumulativeOutputTokens,
          created: task.cumulativeCacheCreationInputTokens,
          read: task.cumulativeCacheReadInputTokens,
          cost: task.costUSD,
        };
      };
      const message = (thread: string, item: string, kind: string) =>
        page.locator(`[data-message-key='${JSON.stringify([thread, "review-turn", item, kind])}']`);
      const answerA = message("child-a", "polygon", "text");
      const answerB = message("child-b", "assembly", "text");
      const discarded = message("child-a", "discarded", "text");
      const reasoning = message("parent", "reasoning", "thinking");

      await expect(cards).toHaveCount(2);
      await cardB.locator(":scope > summary").press("Enter");
      await cardA.locator(":scope > summary").press("Enter");
      await expect(answerA).toContainText("A bow");
      await expect(answerB).toContainText("Assembly draft");
      await expect(discarded).toContainText("Discarded draft");
      await expect(reasoning.locator("details")).not.toHaveAttribute("open");
      const originalCard = await cardA.elementHandle();
      const originalMessage = await answerA.elementHandle();
      expect(originalCard).not.toBeNull();
      expect(originalMessage).not.toBeNull();

      await groupB.locator(":scope > summary").press("Enter");
      await groupA.locator(":scope > summary").press("Enter");
      await toolA.locator(":scope > summary").press("Enter");
      await expect(toolA).toContainText("child-a output start");
      await expect(toolB).not.toContainText("child-a output start");
      const originalTool = await toolA.elementHandle();
      expect(originalTool).not.toBeNull();
      const initial = await api.getTask(id);
      expect(initial.state).toBe("running");
      expect(initial.contextWindowLimit).toBe(200000);
      expect(initial.reportedModel).toBe("root-model");
      expect(initial.activeInputTokens).toBe(100);

      // The fake agent is blocked on stdin. Real input releases another batch
      // while the first turn is running, so this cannot race a sleep duration.
      await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS_CONTINUE" } });
      await expect(answerA.locator("li")).toHaveText([
        "A bowtie uses Earcut.",
        "Validate contours before triangulation.",
      ]);
      await expect(answerA.locator("code")).toHaveText("Earcut");
      await expect(answerB).toContainText("Assembly draft remains in flight.");
      await expect(discarded).toBeEmpty();
      await expect(reasoning.locator("pre")).toHaveText("Considering validation while children research.");
      await expect(reasoning.locator("pre")).not.toBeVisible();
      await expect(cardA).not.toContainText("Assembly draft");
      await expect(cardB).not.toContainText("bowtie");
      await expect(cardA).not.toContainText("Considering validation");
      await expect(toolA.locator(":scope > summary")).toBeFocused();
      expect(await cardA.evaluate((el, original) => el === original, originalCard)).toBe(true);
      expect(await answerA.evaluate((el, original) => el === original, originalMessage)).toBe(true);
      await expect(cardA).toHaveAttribute("open", "");

      await expect(groupA.locator(":scope > summary")).toContainText("0/1 tools");
      await expect(groupB.locator(":scope > summary")).toContainText("0/1 tools");
      await expect(toolA.locator("pre").last()).toHaveText(
        "child-a output start\n" + ".".repeat(1000) + "\nchild-a output complete",
      );
      expect(await toolA.evaluate((el, original) => el === original, originalTool)).toBe(true);
      await expect(toolA).toHaveAttribute("open", "");
      await expect(cardB.locator(":scope > summary")).toContainText("Running");
      expect((await api.getTask(id)).state).toBe("running");

      // Hold the browser's reconnect before opening upstream. Observe the
      // missed batch through a separate real SSE reader before releasing it.
      const requestsBeforeDrop = cursors.length;
      reconnectGate = Promise.withResolvers<void>();
      for (const response of connections) response.destroy();
      await expect.poll(() => cursors.length).toBeGreaterThan(requestsBeforeDrop);
      const cursor = cursors.at(-1);
      expect(cursor).not.toBe("");
      if (!cursor) throw new Error("Missing browser reconnect cursor");
      await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS_RECONNECT" } });
      const markers = ["child-a missed output", "child-b missed output", "parent missed output"];
      const observed = await fetch(
        `${testInfo.project.use.baseURL}/api/caic/v1/tasks/${id}/history/events?cursor=${encodeURIComponent(cursor)}`,
        { signal: AbortSignal.timeout(10000) },
      );
      expect(observed.ok).toBe(true);
      if (!observed.body) throw new Error("Missing SSE observation body");
      const reader = observed.body.getReader();
      const decoder = new TextDecoder();
      const seen = new Set<string>();
      let tail = "";
      try {
        while (seen.size !== markers.length) {
          const { value, done } = await reader.read();
          if (done) throw new Error("SSE ended before the missed output batch");
          const text = tail + decoder.decode(value, { stream: true });
          for (const marker of markers) if (text.includes(marker)) seen.add(marker);
          tail = text.slice(-100);
        }
      } finally {
        await reader.cancel();
      }
      await expect(toolA).not.toContainText("child-a missed output");
      await expect(toolB).not.toContainText("child-b missed output");
      await expect(rootTool).not.toContainText("parent missed output");
      reconnectGate.resolve();
      reconnectGate = null;
      await expect(toolA.locator("pre").last()).toHaveText(
        "child-a output start\n" + ".".repeat(1000) + "\nchild-a output complete\nchild-a missed output",
      );
      await expect(toolB.locator("pre").last()).toHaveText("child-b output start\n\nchild-b missed output");
      await expect(rootTool.locator("pre").last()).toHaveText("parent output start\n\nparent missed output");
      await expect(toolA).not.toContainText("child-b missed output");
      await expect(toolB).not.toContainText("parent missed output");
      await expect(rootTool).not.toContainText("child-a missed output");
      await expect(toolA.locator(":scope > summary")).toBeFocused();
      expect(await toolA.evaluate((el, original) => el === original, originalTool)).toBe(true);
      await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS_A_COMPLETE" } });
      await expect(groupA.locator(":scope > summary")).toContainText("1/1 tools");
      await expect(groupB.locator(":scope > summary")).toContainText("0/1 tools");
      expect((await api.getTask(id)).state).toBe("running");
      await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS_FINISH" } });
      await expect(cardA.locator(":scope > summary")).toContainText("Completed");
      await expect(cardB.locator(":scope > summary")).toContainText("Failed");
      expect((await api.getTask(id)).state).toBe("running");
      await expect(toolA.locator(":scope > summary")).toBeFocused();
      await expect(cardB).toContainText("Assembly command failed");
      await expect(cardB).toContainText("Assembly child failed independently");
      await expect(cardA.getByTestId("orphan-tool")).toHaveCount(1);
      await expect(cardB.getByTestId("orphan-tool")).toHaveCount(1);
      await cardA.getByTestId("orphan-tool").locator("summary").press("Enter");
      await cardB.getByTestId("orphan-tool").locator("summary").press("Enter");
      await expect(cardA).toContainText("Unmatched result remains visible");
      await expect(cardB).toContainText("Unmatched output remains visible");
      await toolA.locator(":scope > summary").focus();
      await api.sendInput(id, { prompt: { text: "FAKE_CONCURRENT_STREAMS_ROOT_FINISH" } });
      await waitForTaskState(api, id, "waiting");
      await expect(cardA.locator(":scope > summary")).toContainText("Completed");
      await expect(cardB.locator(":scope > summary")).toContainText("Failed");
      await expect(answerB).toContainText("Assembly frames preserve local transforms.");
      await expect(cardB).not.toContainText("Assembly draft");
      await expect(cardB.getByText("Assembly frames preserve local transforms.", { exact: true })).toHaveCount(1);
      await expect(cardA.getByText("Polygon review finished; evidence preserved.", { exact: true })).toBeVisible();
      await expect(toolA.locator(":scope > summary")).toBeFocused();
      expect(await cardA.evaluate((el, original) => el === original, originalCard)).toBe(true);
      expect(await answerA.evaluate((el, original) => el === original, originalMessage)).toBe(true);
      await expect(cardA).toHaveAttribute("open", "");
      await expect(page.getByText("Parent summary remains separate.", { exact: true })).toBeVisible();

      const expectedTotals = { input: 123, output: 45, created: 6, read: 78, cost: 0.37 };
      expect(await totals()).toEqual(expectedTotals);
      expect((await api.getTask(id)).reportedModel).toBe("root-model");
      await expect(page.getByTestId("tool-call")).toHaveCount(3);

      await cardA.locator(":scope > summary").press("Enter");
      await expect(answerA).toHaveCount(0);
      await expect(toolA).toHaveCount(0);
      await expect(cardA.locator(":scope > summary")).toContainText("Completed");
      await cardA.locator(":scope > summary").press("Enter");
      await expect(answerA.locator("li")).toHaveText([
        "A bowtie uses Earcut.",
        "Validate contours before triangulation.",
      ]);

      await page.reload();
      await expect(page.getByTestId("task-message-area")).toHaveAttribute("aria-busy", "false");
      // The complete backlog returns automatically on reload, without
      // scrolling or otherwise asking the conversation to load older records.
      await expect(page.getByText("Setup logs", { exact: true })).toHaveCount(1);
      await expect(cards).toHaveCount(2);
      await cardB.locator(":scope > summary").press("Enter");
      await cardA.locator(":scope > summary").press("Enter");
      await expect(answerA.locator("li")).toHaveText([
        "A bowtie uses Earcut.",
        "Validate contours before triangulation.",
      ]);
      await expect(answerA.locator("code")).toHaveText("Earcut");
      await expect(discarded).toBeEmpty();
      await expect(cardB.getByText("Assembly frames preserve local transforms.", { exact: true })).toHaveCount(1);
      await expect(cardA.getByText("Polygon review finished; evidence preserved.", { exact: true })).toBeVisible();
      await expect(reasoning.locator("details")).not.toHaveAttribute("open");
      await expect(reasoning.locator("pre")).toHaveText("Considering validation while children research.");
      for (const card of await cards.all()) {
        expect(await card.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(width);
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
      await expect(cardA.locator(":scope > summary")).toContainText("Completed");
      await expect(cardB.locator(":scope > summary")).toContainText("Failed");
      expect(await totals()).toEqual(expectedTotals);
      await expect(page.getByTestId("tool-call")).toHaveCount(3);
      await groupA.locator(":scope > summary").press("Enter");
      await groupB.locator(":scope > summary").press("Enter");
      await toolA.locator(":scope > summary").press("Enter");
      await toolB.locator(":scope > summary").press("Enter");
      await expect(toolA.locator("pre").last()).toHaveText(
        "child-a output start\n" + ".".repeat(1000) + "\nchild-a output complete\nchild-a missed output",
      );
      await expect(toolA).not.toContainText("parent output");
      await expect(toolB).toContainText("Assembly command failed");
      await expect(rootTool).not.toContainText("child-a output");
      await toolA.locator(":scope > summary").focus();
      await cardA.scrollIntoViewIfNeeded();
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
      await page.screenshot({ path: testInfo.outputPath("concurrent-streams.png"), fullPage: true });
      await toolB.locator(":scope > summary").focus();
      await cardB.scrollIntoViewIfNeeded();
      await expect(toolB.locator(":scope > summary")).toBeFocused();
      expect(await cardB.evaluate((el) => el.getBoundingClientRect().right)).toBeLessThanOrEqual(width);
      await page.screenshot({ path: testInfo.outputPath("concurrent-streams-failure.png"), fullPage: true });
      expect(appErrors).toEqual([]);
    } finally {
      reconnectGate?.resolve();
      for (const response of connections) response.destroy();
      proxy.closeAllConnections();
      await new Promise<void>((resolve, reject) => proxy.close((error) => (error ? reject(error) : resolve())));
    }
  });
}
