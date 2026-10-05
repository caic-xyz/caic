// Browser coverage for true-depth continuous process trees, bottom-aligned headers, and keyboard disclosures.

import type { ISOTimestamp, ProcessInfo } from "../../sdk/caic/ts/v1/types.gen";
import { createTaskAPI, expect, test } from "../helpers";

test("process tree keeps actions aligned and commands readable at desktop and mobile widths", async ({ page, api }) => {
  const id = await createTaskAPI(api, "Inspect process layout");
  const startedAt = new Date().toISOString() as ISOTimestamp;
  const commands: [number, number, string][] = [
    [1, 0, "/sbin/init"],
    [2, 1, "/usr/bin/bash -lc pnpm test"],
    [3, 2, "/usr/bin/node " + "long-command-argument/".repeat(70)],
    [4, 1, "/usr/bin/sleep 100"],
    [5, 3, "/usr/bin/python3 --version"],
    [6, 5, "/usr/bin/worker --idle"],
  ];
  const processes: ProcessInfo[] = commands.map(([pid, ppid, command]) => ({
    pid,
    ppid,
    command,
    pgrp: 1,
    user: "user",
    state: "S",
    priority: 20,
    nice: 0,
    threads: 1,
    cpu: 0,
    mem: 0,
    rssBytes: 0,
    cpuTime: 0,
    startedAt,
  }));
  await page.route(`**/api/caic/v1/processes/${id}`, (route) => route.fulfill({ json: { processes } }));
  await page.setViewportSize({ width: 2400, height: 1000 });
  await page.goto(`/task/@${id}/processes`);
  const rows = page.getByRole("row");
  await expect(rows).toHaveCount(7);
  await expect(rows.nth(3).getByRole("cell").first()).toContainText("node");
  const headerBottoms = await page.getByRole("columnheader").evaluateAll((headers) =>
    headers.map((header) => {
      const range = document.createRange();
      range.selectNodeContents(header);
      return range.getBoundingClientRect().bottom;
    }),
  );
  expect(Math.max(...headerBottoms) - Math.min(...headerBottoms)).toBeLessThan(1);
  const lanePositions = await Promise.all(
    ["node", "python3", "worker"].map(async (name) => (await page.getByText(name, { exact: true }).boundingBox())?.x),
  );
  expect(lanePositions[1]!).toBeGreaterThan(lanePositions[0]!);
  expect(lanePositions[2]!).toBeGreaterThan(lanePositions[1]!);
  expect(lanePositions[2]! - lanePositions[1]!).toBeCloseTo(lanePositions[1]! - lanePositions[0]!, 1);
  const wrappedRow = rows.nth(3);
  const lane = wrappedRow.getByTestId("process-graph").locator("span").first();
  const nextLane = rows.nth(4).getByTestId("process-graph").locator("span").first();
  const rowBox = await wrappedRow.boundingBox();
  const laneBox = await lane.boundingBox();
  const nextBox = await nextLane.boundingBox();
  expect(rowBox).not.toBeNull();
  expect(laneBox).not.toBeNull();
  expect(nextBox).not.toBeNull();
  expect(rowBox!.height).toBeGreaterThan(60);
  expect(laneBox!.y).toBeLessThanOrEqual(rowBox!.y);
  expect(laneBox!.y + laneBox!.height).toBeGreaterThanOrEqual(nextBox!.y);
  expect(laneBox!.x).toBe(nextBox!.x);
  const stroke = await lane.evaluate((el) => {
    const style = getComputedStyle(el, "::before");
    return { top: style.top, bottom: style.bottom, border: style.borderLeftWidth };
  });
  expect(stroke).toEqual({ top: "0px", bottom: "0px", border: "1px" });
  const childStroke = await wrappedRow.getByTestId("process-child-lane").evaluate((el) => {
    const box = el.getBoundingClientRect();
    return { x: box.x, bottom: box.bottom, top: box.top + Number.parseFloat(getComputedStyle(el, "::before").top) };
  });
  const childConnection = await rows.nth(4).getByTestId("process-parent-lane").boundingBox();
  expect(childConnection).not.toBeNull();
  expect(childStroke.x).toBe(childConnection!.x);
  expect(childStroke.top).toBeGreaterThan(rowBox!.y);
  expect(childStroke.top).toBeLessThan(childConnection!.y);
  expect(childStroke.bottom).toBeGreaterThanOrEqual(childConnection!.y);
  const buttons = page.getByTitle("Send SIGTERM (graceful termination)");
  const positions = await buttons.evaluateAll((els) => els.map((el) => el.getBoundingClientRect().left));
  expect(new Set(positions).size).toBe(1);
  const command = rows.nth(3).getByRole("cell").last().locator("div");
  expect((await command.boundingBox())?.width).toBeGreaterThanOrEqual(700);
  await page.setViewportSize({ width: 4000, height: 1000 });
  expect((await command.boundingBox())?.width).toBeLessThanOrEqual(1400);
  await page.setViewportSize({ width: 2400, height: 1000 });
  await expect(rows.nth(1).getByRole("cell").nth(14)).toHaveText(
    new Date(startedAt).toLocaleTimeString("en-US", { timeZone: "UTC" }),
  );
  await page.screenshot({ path: "test-results/processes-desktop.png" });
  await page.getByRole("button", { name: "Collapse init children (PID 1)" }).focus();
  await page.keyboard.press("Enter");
  await expect(rows).toHaveCount(2);
  await expect(page.getByRole("button", { name: "Expand init children (PID 1)" })).toBeFocused();
  await page.keyboard.press("Space");
  await expect(rows).toHaveCount(7);

  await page.setViewportSize({ width: 390, height: 844 });
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await expect(rows.nth(3).getByRole("cell").first()).toBeVisible();
  await page.screenshot({ path: "test-results/processes-mobile.png" });
  await page.setViewportSize({ width: 390, height: 300 });
  await page.getByText("worker", { exact: true }).scrollIntoViewIfNeeded();
  const processHeader = page.getByRole("columnheader", { name: "PROCESS", exact: true });
  expect(
    await processHeader.evaluate((el) => {
      const box = el.getBoundingClientRect();
      return el.contains(document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2));
    }),
  ).toBe(true);
});
