// Browser coverage for always-visible CPU/memory levels, live SSE readings, stream liveness, and header containment.

import { createServer, type ServerResponse } from "node:http";
import type { ServerHealth, TaskListEvent, ISOTimestamp } from "../../sdk/caic/ts/v1/types.gen";
import { expect, test } from "../helpers";

for (const width of [320, 390, 1280]) {
  test(`live server usage levels at ${width}px`, async ({ page }, testInfo) => {
    const errors: string[] = [];
    const requests: string[] = [];
    page.on("pageerror", (e) => errors.push(e.message));
    page.on("request", (r) => requests.push(r.url()));
    await page.setViewportSize({ width, height: 844 });
    await page.clock.install();
    await page.clock.pauseAt(new Date());
    const clients = new Set<ServerResponse>();
    let health: ServerHealth = {
      time: "2026-01-01T00:00:00Z" as ISOTimestamp,
      cpuPerc: 750,
      cpuCount: 8,
      memUsed: 30 * 2 ** 30,
      memTotal: 32 * 2 ** 30,
      cpuHigh: true,
      memHigh: true,
    };
    const send = (response: ServerResponse, event: TaskListEvent) =>
      response.write(`data: ${JSON.stringify(event)}\n\n`);
    // A real open SSE response allows deterministic live updates and actual
    // EventSource disconnect handling, without changing application state/DOM.
    const server = createServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "text/event-stream", "Access-Control-Allow-Origin": "*" });
      clients.add(response);
      response.on("close", () => clients.delete(response));
      send(response, { kind: "snapshot", snapshot: [], complete: true });
      send(response, { kind: "health", health });
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("Fixture server did not start");
    const update = (patch: Partial<ServerHealth>) => {
      health = { ...health, ...patch };
      for (const client of clients) send(client, { kind: "health", health });
    };
    try {
      await page.route("**/api/caic/v1/tasks/events", (route) =>
        route.fulfill({ status: 307, headers: { location: `http://127.0.0.1:${address.port}/events` } }),
      );
      await page.goto("/");
      const link = page.getByRole("link", { name: /CPU .*memory .* — open System/ });
      await expect(link).toBeVisible();
      const cpu = page.getByTestId("cpu-alarm");
      const memory = page.getByTestId("memory-alarm");
      const avatar = page.getByRole("button", { name: "Menu" });
      const before = await avatar.boundingBox();
      const cpuBox = await cpu.boundingBox();
      const memBox = await memory.boundingBox();
      const linkBox = await link.boundingBox();
      if (!before || !cpuBox || !memBox || !linkBox) throw new Error("Header alarms missing");
      expect(cpuBox.height).toBe(12);
      expect(memBox.height).toBe(12);
      expect(memBox.y).toBe(cpuBox.y + cpuBox.height);
      expect(linkBox.height).toBeGreaterThanOrEqual(28);
      expect(linkBox.width).toBeGreaterThanOrEqual(28);
      expect(linkBox.x + linkBox.width).toBeLessThanOrEqual(before.x);
      expect(before.x - (cpuBox.x + cpuBox.width)).toBeLessThanOrEqual(8);
      await expect(cpu).toHaveCSS("fill", "rgb(220, 53, 69)");
      await expect(memory).toHaveCSS("fill", "rgb(220, 53, 69)");
      await avatar.focus();
      await link.hover();
      const tooltip = page.getByRole("tooltip");
      await expect(tooltip).toContainText("CPU: 750.0% · 7.50 / 8 CPUs (93.8%)");
      await expect(tooltip).toContainText("RAM: 30 GiB / 32 GiB (93.8%)");
      const tipBox = await tooltip.boundingBox();
      if (!tipBox) throw new Error("Tooltip missing");
      expect(tipBox.x).toBeGreaterThanOrEqual(0);
      expect(tipBox.x + tipBox.width).toBeLessThanOrEqual(width);
      await page.screenshot({ path: testInfo.outputPath("both-high.png") });
      // Hover must dismiss even while keyboard focus belongs to another control.
      await page.keyboard.press("Escape");
      await expect(tooltip).toBeHidden();
      await expect(avatar).toBeFocused();
      await page.mouse.move(0, 200);
      await link.focus();
      await expect(tooltip).toBeVisible();
      await link.press("Escape");
      await expect(tooltip).toBeHidden();
      await expect(link).toBeFocused();

      update({ cpuPerc: 400 });
      await expect(link).toHaveAccessibleName("CPU normal, memory high — open System");
      await expect(cpu).toHaveCSS("fill", "rgb(40, 167, 69)");
      await expect(memory).toHaveCSS("fill", "rgb(220, 53, 69)");
      expect(await memory.boundingBox()).toEqual(memBox);
      expect(await avatar.boundingBox()).toEqual(before);
      await page.screenshot({ path: testInfo.outputPath("memory-high.png") });
      update({ cpuPerc: 680, memUsed: 16 * 2 ** 30 });
      await expect(link).toHaveAccessibleName("CPU warning, memory normal — open System");
      await expect(cpu).toHaveCSS("fill", "rgb(184, 92, 0)");
      await expect(memory).toHaveCSS("fill", "rgb(40, 167, 69)");
      expect(await cpu.boundingBox()).toEqual(cpuBox);
      await page.screenshot({ path: testInfo.outputPath("cpu-warning.png") });
      update({ cpuPerc: 0, memUsed: 0 });
      await expect(link).toHaveAccessibleName("CPU normal, memory normal — open System");
      await expect(cpu).toBeVisible();
      await expect(memory).toBeVisible();
      await expect(cpu).toHaveCSS("fill", "rgb(40, 167, 69)");
      await expect(memory).toHaveCSS("fill", "rgb(40, 167, 69)");
      await page.screenshot({ path: testInfo.outputPath("both-normal.png") });
      update({ error: "sample failed" });
      await expect(link).toHaveAccessibleName("CPU unavailable, memory unavailable — open System");
      await expect(cpu).toHaveCSS("fill", "rgb(108, 117, 125)");
      await expect(memory).toHaveCSS("fill", "rgb(108, 117, 125)");
      await avatar.focus();
      await link.focus();
      await expect(tooltip).toContainText("Sampling error: sample failed");
      expect(await avatar.boundingBox()).toEqual(before);
      update({ error: "", cpuPerc: 750, memUsed: 30 * 2 ** 30 });
      await expect(link).toBeVisible();
      await page.clock.fastForward(30_000);
      await expect(link).toBeVisible();
      for (const client of clients) send(client, { kind: "heartbeat" });
      await expect(page.getByTestId("new-task-button")).toHaveAttribute("data-status", "connected");
      await page.clock.fastForward(30_000);
      await expect(link).toBeVisible();
      expect(requests.filter((url) => url.includes("/server/system-stats"))).toHaveLength(0);
      // One shared task stream, plus the existing usage stream. Health adds none.
      expect(requests.filter((url) => url.endsWith("/tasks/events"))).toHaveLength(1);
      expect(requests.filter((url) => url.endsWith("/usage/events"))).toHaveLength(1);
      await page.clock.fastForward(15_000);
      await expect(page.getByTestId("new-task-button")).toHaveAttribute("data-status", "disconnected");
      await expect(link).toBeVisible();
      await link.focus();
      await expect(tooltip).toContainText("Updates unavailable");
      await expect(cpu).toHaveCSS("fill", "rgb(108, 117, 125)");
      await expect(memory).toHaveCSS("fill", "rgb(108, 117, 125)");
      await page.screenshot({ path: testInfo.outputPath("disconnected.png") });
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      await link.focus();
      await link.press("Enter");
      await expect(page).toHaveURL(/\/monitoring\?section=system$/);
      await expect(page.getByRole("heading", { name: "System", exact: true })).toBeVisible();
      expect(errors).toEqual([]);
    } finally {
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
    }
  });
}
