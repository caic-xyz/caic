// Exercises STEP indexing before network completion, cancellation, and late archive validation over gated HTTP streams.
import { ok } from "node:assert/strict";
import { createServer, type ServerResponse } from "node:http";
import { once } from "node:events";
import { zipSync } from "three/addons/libs/fflate.module.js";
import { createFacetedStep } from "../../frontend/tests/step-fixture";
import { test, expect, createTaskAPI, waitForTaskState, collectStepTimings } from "../helpers";

async function archiveStream(corrupt: boolean) {
  const archive = Buffer.from(zipSync({ "assembly.stp": createFacetedStep(8, false) }));
  const invalid = Buffer.from(archive);
  const directory = archive.readUInt32LE(archive.length - 6);
  invalid.writeUInt32LE((invalid.readUInt32LE(directory + 16) ^ 1) >>> 0, directory + 16);
  const held = new Map<ServerResponse, Buffer>();
  let requests = 0;
  const server = createServer((_request, response) => {
    requests++;
    const bytes = corrupt && requests === 1 ? invalid : archive;
    response.writeHead(200, {
      "Access-Control-Allow-Origin": "*",
      "Content-Type": "application/zip",
      "Content-Length": bytes.length,
      "Cache-Control": "no-store",
    });
    if (corrupt && requests > 1) {
      response.end(bytes);
      return;
    }
    // On the integrity journey the entire STEP is available, but the directory
    // and its authoritative checksum are withheld. No model may be published.
    const split = corrupt ? directory : Math.floor(bytes.length / 2);
    response.write(bytes.subarray(0, split));
    held.set(response, bytes.subarray(split));
    response.on("close", () => held.delete(response));
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Missing archive stream address");
  return {
    url: `http://127.0.0.1:${address.port}/assembly.stpz`,
    finish() {
      for (const [response, remaining] of held) response.end(remaining);
      held.clear();
    },
    async close() {
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
    },
  };
}

test.describe("streaming STEP", () => {
  test.use({ serviceWorkers: "block" });
  for (const { width, corrupt } of [
    { width: 390, corrupt: false },
    { width: 1280, corrupt: true },
  ]) {
    test(`${corrupt ? "rejects a late checksum failure" : "cancels and retries an unfinished stream"} at ${width}px`, async ({
      page,
      context,
      api,
      uniquePrompt,
    }) => {
      const source = await archiveStream(corrupt);
      try {
        await page.setViewportSize({ width, height: 900 });
        const errors: string[] = [];
        page.on("pageerror", (error) => errors.push(error.message));
        page.on("console", (message) => {
          if (message.type() === "error") errors.push(message.text());
        });
        const diagnostics = await collectStepTimings(page);
        await context.route("**/raw?*", (route) => route.fulfill({ status: 307, headers: { Location: source.url } }));
        const id = await createTaskAPI(api, uniquePrompt("Inspect [Assembly](/assembly.stpz)."));
        await waitForTaskState(api, id, "waiting");
        await page.goto(`/task/@${id}`);
        const started = page.waitForEvent("worker", (worker) => worker.url().includes("stepWorker"));
        await page.getByRole("link", { name: "Assembly", exact: true }).click();
        const worker = await started;
        await expect(page.getByRole("progressbar", { name: "Downloading and reading STEP" })).toBeVisible();
        await expect(page.getByText(/[1-9][\d,]* records indexed/)).toBeVisible();
        await expect(page.getByText("32 triangles", { exact: true })).toHaveCount(0);
        expect(diagnostics).toHaveLength(0);
        await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        await page.screenshot({ path: test.info().outputPath("streamed-step-reading.png") });
        if (corrupt) {
          source.finish();
          await expect(page.getByRole("alert")).toContainText("CRC");
          expect(diagnostics).toHaveLength(0);
          await expect(page.getByText("32 triangles", { exact: true })).toHaveCount(0);
          await page.getByRole("button", { name: "Retry", exact: true }).click();
        } else {
          const closed = new Promise<void>((resolve) => worker.once("close", () => resolve()));
          await page.getByRole("button", { name: "Cancel loading", exact: true }).click();
          await closed;
          await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeFocused();
          await page.getByRole("button", { name: "Retry", exact: true }).click();
          await expect(page.getByText(/[1-9][\d,]* records indexed/)).toBeVisible();
          source.finish();
        }
        await expect(page.getByText("32 triangles", { exact: true })).toBeVisible();
        await expect.poll(() => diagnostics.length).toBe(1);
        ok(diagnostics[0]);
        expect(diagnostics[0].stages.occtParseMs).toBe(0);
        await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        await page.screenshot({ path: test.info().outputPath("streamed-step.png") });
        expect(errors).toEqual([]);
      } finally {
        await source.close();
      }
    });
  }
});
