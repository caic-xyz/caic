// Browser coverage for recent task previews, gated canonical backfill, and live handoff.
import { once } from "node:events";
import { createServer, request as httpRequest } from "node:http";

import { toolIdentity } from "../../frontend/src/grouping";
import { validateTaskEventBackward } from "../../sdk/caic/ts/v1/validate.gen";
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`recent task context renders before backfill at ${width}px`, async ({ page, api, uniquePrompt }, testInfo) => {
    const id = await createTaskAPI(api, uniquePrompt("FAKE_DEMO"));
    await waitForTaskState(api, id, "waiting");
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const release = Promise.withResolvers<void>();
    const preview = Promise.withResolvers<ReturnType<typeof validateTaskEventBackward>>();
    const proxy = createServer((request, response) => {
      const upstream = httpRequest(
        `${testInfo.project.use.baseURL}${request.url}`,
        { headers: { ...request.headers, "accept-encoding": "identity" } },
        async (stream) => {
          response.writeHead(stream.statusCode ?? 502, {
            ...stream.headers,
            "access-control-allow-origin": String(testInfo.project.use.baseURL),
            "access-control-allow-credentials": "true",
          });
          stream.on("error", (error) => response.destroy(error));
          if (!request.url?.includes("backward=1")) {
            stream.pipe(response);
            return;
          }
          // Forward the real preview immediately, then hold all canonical frames.
          // No task event is replaced or synthesized by this proxy.
          let buffer = "";
          try {
            for await (const chunk of stream.iterator({ destroyOnReturn: false })) {
              buffer += String(chunk);
              const start = buffer.indexOf("event: backward\n");
              const end = start < 0 ? -1 : buffer.indexOf("\n\n", start);
              if (end < 0) continue;
              const data = buffer.slice(start, end).split("\ndata: ")[1];
              if (!data) throw new Error("Missing preview payload");
              preview.resolve(validateTaskEventBackward(JSON.parse(data)));
              response.write(buffer.slice(0, end + 2));
              await release.promise;
              if (response.destroyed) return;
              response.write(buffer.slice(end + 2));
              stream.pipe(response);
              return;
            }
            throw new Error("Task stream ended without a preview");
          } catch (error) {
            preview.reject(error);
            response.destroy(error instanceof Error ? error : new Error(String(error)));
          }
        },
      );
      response.on("close", () => upstream.destroy());
      upstream.on("error", (error) => response.destroy(error));
      upstream.end();
    });
    proxy.listen(0, "127.0.0.1");
    await once(proxy, "listening");
    const address = proxy.address();
    if (!address || typeof address === "string") throw new Error("Missing proxy address");
    await page.route(`**/tasks/${id}/events?*`, (route) => {
      const url = new URL(route.request().url());
      url.port = String(address.port);
      return route.continue({ url: url.toString() });
    });
    try {
      await page.goto(`/task/@${id}`);
      const batch = await preview.promise;
      expect(batch.messages.length).toBeGreaterThan(5);
      expect(batch.eventId).not.toBe("");
      const texts = batch.messages.flatMap((event) => (event.text?.text ? [event.text.text] : []));
      expect(texts.length).toBeGreaterThan(1);
      const first = texts[0];
      if (!first) throw new Error("Missing preview text");
      await expect(page.getByText(first, { exact: true })).toBeVisible();
      const tool = batch.messages.find((event) => event.toolUse);
      if (!tool?.toolUse) throw new Error("Missing preview tool");
      await expect(page.locator(`[data-tool-key='${toolIdentity(tool.source, tool.toolUse.toolUseID)}']`)).toHaveCount(
        1,
      );
      await page.screenshot({ path: testInfo.outputPath(`preview-${width}.png`) });
      release.resolve();
      // A follow-up travels over the actual tail connection after backfill.
      const input = uniquePrompt("preview follow-up");
      await api.sendInput(id, { prompt: { text: input } });
      await expect(page.getByText(input, { exact: true })).toBeVisible();
      await waitForTaskState(api, id, "waiting");
      // Completion collapses the prior turn. Expand it before checking that
      // canonical backfill and the tail did not duplicate the preview.
      await page.getByRole("button", { name: /2 messages, 4 tool calls/ }).click();
      await expect(page.getByText(first, { exact: true })).toHaveCount(1);
      expect(errors).toEqual([]);
    } finally {
      release.resolve();
      proxy.closeAllConnections();
      await new Promise<void>((resolve, reject) => proxy.close((error) => (error ? reject(error) : resolve())));
    }
  });
}
