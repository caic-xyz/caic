// Browser coverage for silent open SSE stalls and conversation continuity through failed recovery.
import { once } from "node:events";
import { createServer, request as httpRequest, type ServerResponse } from "node:http";
import { createTaskAPI, expect, test, waitForTaskState } from "../helpers";

test("silent streams recover without replacing the conversation being read", async ({
  page,
  api,
  uniquePrompt,
}, testInfo) => {
  const id = await createTaskAPI(api, uniquePrompt("FAKE_DEMO"));
  await waitForTaskState(api, id, "waiting");
  await page.setViewportSize({ width: 1280, height: 400 });
  await page.clock.install();
  await page.clock.pauseAt(new Date());
  const clients = new Map<ServerResponse, string>();
  const requests: string[] = [];
  let paused = false;
  let holdNativeReconnect = false;
  let nativeReconnects = 0;
  const proxy = createServer((request, response) => {
    const path = request.url ?? "";
    requests.push(path);
    clients.set(response, path);
    if (holdNativeReconnect && request.headers["last-event-id"] && path.includes("/history/events?")) {
      nativeReconnects++;
      response.on("close", () => clients.delete(response));
      // Keep the native reconnect waiting for response headers indefinitely.
      return;
    }
    const upstream = httpRequest(
      `${testInfo.project.use.baseURL}${path}`,
      { headers: { ...request.headers, "accept-encoding": "identity" } },
      (stream) => {
        response.writeHead(stream.statusCode ?? 502, {
          ...stream.headers,
          "access-control-allow-origin": String(testInfo.project.use.baseURL),
          "access-control-allow-credentials": "true",
        });
        response.flushHeaders();
        stream.on("data", (chunk: Buffer) => {
          // Continue consuming upstream while the browser's connection remains
          // open but silent: no EOF/error can trigger native EventSource retry.
          if (!paused || path.endsWith("/tasks/events")) response.write(chunk);
        });
        stream.on("error", (error) => response.destroy(error));
      },
    );
    response.on("close", () => {
      clients.delete(response);
      upstream.destroy();
    });
    upstream.on("error", (error) => response.destroy(error));
    upstream.end();
  });
  proxy.listen(0, "127.0.0.1");
  await once(proxy, "listening");
  const address = proxy.address();
  if (!address || typeof address === "string") throw new Error("Missing proxy port");
  await page.route(/\/api\/caic\/v1\/(?:tasks\/events|usage\/events|tasks\/[^/]+\/history\/events\?)/, (route) => {
    const url = new URL(route.request().url());
    url.port = String(address.port);
    return route.continue({ url: url.toString() });
  });
  const gate = Promise.withResolvers<void>();
  let recovering = false;
  let gated = false;
  let fail = true;
  await page.route(`**/tasks/${id}/history/manifest`, async (route) => {
    if (!recovering) return route.continue();
    gated = true;
    await gate.promise;
    return fail ? route.fulfill({ status: 503, body: "temporarily unavailable" }) : route.continue();
  });
  const setupGate = Promise.withResolvers<void>();
  let setupRequested = false;
  await page.route(`**/tasks/${id}/history/**/setup/*`, async (route) => {
    setupRequested = true;
    await setupGate.promise;
    return route.continue();
  });
  try {
    await page.goto(`/task/@${id}`);
    const area = page.getByTestId("task-message-area");
    await expect(area).toHaveAttribute("aria-busy", "false");
    const draft = page.getByTestId("task-detail-prompt");
    await draft.fill("Keep this unsent draft");
    const setup = page.getByTestId("task-setup");
    await setup.locator(":scope > summary").click();
    await expect.poll(() => setupRequested).toBe(true);
    await expect(page.getByRole("status", { name: "Loading setup logs" })).toBeVisible();
    // Mark scrolling as user-owned before moving away from the latest output.
    await area.hover();
    await area.evaluate((element) => {
      element.dispatchEvent(new WheelEvent("wheel", { deltaY: -400 }));
      element.scrollTop = (element.scrollHeight - element.clientHeight) / 2;
      element.dispatchEvent(new Event("scroll"));
    });
    await page.clock.runFor(100);
    const distances = await area.evaluate((element) => ({
      start: element.scrollTop,
      end: element.scrollHeight - element.clientHeight - element.scrollTop,
    }));
    expect(distances.start).toBeGreaterThan(40);
    expect(distances.end).toBeGreaterThan(40);
    const original = (
      await area.evaluateHandle((element) => {
        const viewport = element.getBoundingClientRect();
        return (
          [...element.querySelectorAll<HTMLElement>("[data-anchor-key]")].find((row) => {
            const bounds = row.getBoundingClientRect();
            return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
          }) ?? null
        );
      })
    ).asElement();
    if (!original) throw new Error("Missing conversation anchor");
    const position = await original.evaluate((element) => element.getBoundingClientRect().top);
    expect(await area.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);
    const text = await area.textContent();
    await expect.poll(() => clients.size).toBe(3);
    await page.clock.runFor(100);
    paused = true;
    recovering = true;
    // Task heartbeats keep its independent owner healthy while history and
    // provider usage remain silent for their full watchdog interval.
    await page.clock.fastForward(30_000);
    for (const [response, path] of clients) {
      if (path.endsWith("/tasks/events")) response.write('data: {"kind":"heartbeat"}\n\n');
    }
    await page.clock.runFor(100);
    await page.clock.fastForward(15_000);
    await page.clock.runFor(1000);
    await expect.poll(() => gated).toBe(true);
    await expect(area).toHaveAttribute("aria-busy", "true");
    await expect(area.getByRole("heading", { name: "Prompt", exact: true })).toHaveCount(0);
    expect(await original.evaluate((element) => element.isConnected)).toBe(true);
    expect(await area.textContent()).toContain(text?.trim() ?? "");
    expect(
      Math.abs((await original.evaluate((element) => element.getBoundingClientRect().top)) - position),
    ).toBeLessThan(1);
    await expect.poll(() => requests.filter((path) => path.endsWith("/usage/events")).length).toBe(2);
    gate.resolve();
    await expect(page.getByRole("button", { name: "Retry history" })).toBeVisible();
    expect(await original.evaluate((element) => element.isConnected)).toBe(true);
    expect(
      Math.abs((await original.evaluate((element) => element.getBoundingClientRect().top)) - position),
    ).toBeLessThan(1);
    fail = false;
    paused = false;
    await page.getByRole("button", { name: "Retry history" }).dispatchEvent("click");
    await expect(area).toHaveAttribute("aria-busy", "false");
    expect(await original.evaluate((element) => element.isConnected)).toBe(true);
    expect(
      Math.abs((await original.evaluate((element) => element.getBoundingClientRect().top)) - position),
    ).toBeLessThan(1);
    await expect(draft).toHaveText("Keep this unsent draft");
    expect(requests.filter((path) => path.endsWith("/tasks/events"))).toHaveLength(1);
    expect(requests.filter((path) => path.includes("/history/events?"))).toHaveLength(2);
    // This disclosure belongs to the published snapshot, so retiring the live
    // connection must neither abort it nor require a second disclosure request.
    await expect(page.getByRole("status", { name: "Loading setup logs" })).toBeVisible();
    setupGate.resolve();
    await expect(page.getByTestId("task-setup-logs")).not.toBeEmpty();
    await expect(setup.getByRole("alert")).toHaveCount(0);

    await page.clock.runFor(100);
    holdNativeReconnect = true;
    for (const [response, path] of clients) if (path.includes("/history/events?")) response.destroy();
    await expect.poll(() => nativeReconnects).toBe(1);
    await page.clock.fastForward(30_000);
    for (const [response, path] of clients) {
      if (path.endsWith("/tasks/events")) response.write('data: {"kind":"heartbeat"}\n\n');
      if (path.endsWith("/usage/events")) response.write('event: heartbeat\ndata: {"time":"2026-01-01T00:00:00Z"}\n\n');
    }
    await page.clock.runFor(100);
    await page.clock.fastForward(15_000);
    await page.clock.runFor(100);
    await expect.poll(() => requests.filter((path) => path.includes("/history/events?")).length).toBe(4);
    await expect(area).toHaveAttribute("aria-busy", "false");
    expect(await original.evaluate((element) => element.isConnected)).toBe(true);
    await expect(draft).toHaveText("Keep this unsent draft");
    await expect(area.getByRole("heading", { name: "Prompt", exact: true })).toHaveCount(0);
  } finally {
    setupGate.resolve();
    gate.resolve();
    for (const response of clients.keys()) response.destroy();
    proxy.closeAllConnections();
    await new Promise<void>((resolve, reject) => proxy.close((error) => (error ? reject(error) : resolve())));
  }
});
