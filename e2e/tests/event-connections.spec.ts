// Browser coverage for independent task/usage reconnects and retained drafts across real SSE transport replacement.

import { createServer, type ServerResponse } from "node:http";
import type { TaskListEvent, UsageResp } from "../../sdk/caic/ts/v1/types.gen";
import { expect, test } from "../helpers";

test("usage reconnects without replacing the task stream or its draft", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.clock.install();
  await page.clock.pauseAt(new Date());
  const taskClients = new Set<ServerResponse>();
  const usageClients = new Set<ServerResponse>();
  let taskConnections = 0;
  let usageConnections = 0;
  const server = createServer((request, response) => {
    response.writeHead(200, { "Content-Type": "text/event-stream", "Access-Control-Allow-Origin": "*" });
    const usage = request.url === "/usage";
    const clients = usage ? usageClients : taskClients;
    clients.add(response);
    response.on("close", () => clients.delete(response));
    if (usage) {
      usageConnections++;
      response.write(`data: ${JSON.stringify({ providers: [], local: { windows: [] } } satisfies UsageResp)}\n\n`);
    } else {
      taskConnections++;
      response.write(
        `data: ${JSON.stringify({ kind: "snapshot", snapshot: [], complete: true } satisfies TaskListEvent)}\n\n`,
      );
    }
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Stream fixture did not start");
  const origin = `http://127.0.0.1:${address.port}`;
  try {
    await page.route("**/api/caic/v1/tasks/events", (route) =>
      route.fulfill({ status: 307, headers: { location: `${origin}/tasks` } }),
    );
    await page.route("**/api/caic/v1/usage/events", (route) =>
      route.fulfill({ status: 307, headers: { location: `${origin}/usage` } }),
    );
    await page.goto("/");
    const status = page.getByTestId("new-task-button");
    await expect(status).toHaveAttribute("data-status", "connected");
    await expect.poll(() => usageClients.size).toBe(1);
    const draft = page.getByTestId("prompt-input");
    await draft.fill("Keep this while usage reconnects");
    const document = await page.evaluate(() => performance.timeOrigin);
    const finished = page.waitForEvent("requestfinished", (request) => request.url() === `${origin}/usage`);
    for (const client of usageClients) client.end();
    await finished;
    await page.clock.runFor(1000);
    await expect.poll(() => usageConnections).toBe(2);
    expect(taskConnections).toBe(1);
    expect(taskClients.size).toBe(1);
    await expect(status).toHaveAttribute("data-status", "connected");
    await expect(draft).toHaveText("Keep this while usage reconnects");
    expect(await page.evaluate(() => performance.timeOrigin)).toBe(document);
    expect(errors).toEqual([]);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
  }
});
