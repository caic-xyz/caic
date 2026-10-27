// Verifies built caic frontend readiness waits for authoritative initial task data.

import { expect, test } from "../helpers";

test("native preparation waits for the empty complete task snapshot", async ({ page }) => {
  const snapshot = Promise.withResolvers<void>();
  const requested = Promise.withResolvers<void>();
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.addInitScript(() => {
    const messages: Array<{ ready: boolean; tools: Array<{ name: string }> }> = [];
    const bridge: { onmessage?: ((event: { data: string }) => void) | null; postMessage(message: string): void } = {
      postMessage(message) {
        const auth = JSON.parse(message) as Record<string, unknown>;
        if (auth.authChanged === true && auth.nativeAccess === true) {
          bridge.onmessage?.({ data: JSON.stringify({ ...auth, frontendToken: "native-identity" }) });
        }
      },
    };
    Object.assign(window, {
      goModeHost: { isVoiceConnected: () => false },
      goModeFrontendDocument: Promise.resolve("native-document"),
      gomodeAuth: bridge,
      gomodeFrontendReadiness: { postMessage: (message: string) => messages.push(JSON.parse(message)) },
      preparationMessages: messages,
    });
  });
  await page.route("**/api/caic/v1/tasks/events", async (route) => {
    requested.resolve();
    await snapshot.promise;
    await route.fulfill({
      contentType: "text/event-stream",
      body: `data: ${JSON.stringify({ kind: "snapshot", snapshot: [], complete: true })}\n\n`,
    });
  });
  await page.goto("/");
  await requested.promise;
  await expect(page.getByTestId("app-shell")).toBeVisible();
  await page.waitForFunction(
    () => typeof (window as unknown as { executeGoModeVoiceTool?: unknown }).executeGoModeVoiceTool === "function",
  );
  const readiness = () =>
    page.evaluate(
      () =>
        (window as unknown as { preparationMessages: Array<{ ready: boolean; tools: Array<{ name: string }> }> })
          .preparationMessages,
    );
  await expect.poll(async () => (await readiness()).length).toBeGreaterThan(0);
  expect((await readiness()).some((message) => message.ready)).toBe(false);
  snapshot.resolve();
  await expect
    .poll(async () => (await readiness()).at(-1))
    .toEqual({
      document: "native-document",
      token: "native-identity",
      ready: true,
      tools: [expect.objectContaining({ name: "focus_task" })],
    });
  expect(errors).toEqual([]);
});

test("browser voice and its keyboard shortcut wait for initial task preparation", async ({ page }, testInfo) => {
  const snapshot = Promise.withResolvers<void>();
  const requested = Promise.withResolvers<void>();
  const voiceRequests: string[] = [];
  page.on("request", (request) => {
    if (request.url().includes("/api/voicegateway/")) voiceRequests.push(request.url());
  });
  await page.route("**/api/caic/v1/server/config", async (route) => {
    const response = await route.fetch();
    const config = await response.json();
    await route.fulfill({ response, json: { ...config, voiceGateway: { mode: "embedded" } } });
  });
  await page.route("**/.well-known/gomode.json", async (route) => {
    const response = await route.fetch();
    const manifest = await response.json();
    manifest.webShell.voiceGateway = { url: "/" };
    await route.fulfill({ response, json: manifest });
  });
  await page.route("**/api/voicegateway/v1/voice/choices", (route) =>
    route.fulfill({
      json: { choices: [{ id: "test", label: "Test voice", transports: ["webrtc"] }], defaultChoiceID: "test" },
    }),
  );
  await page.route("**/api/caic/v1/tasks/events", async (route) => {
    requested.resolve();
    await snapshot.promise;
    await route.fulfill({
      contentType: "text/event-stream",
      body: `data: ${JSON.stringify({ kind: "snapshot", snapshot: [], complete: true })}\n\n`,
    });
  });
  await page.goto("/");
  await requested.promise;
  await expect(page.getByTestId("app-shell")).toBeVisible();
  await expect(page.getByTestId("voice-overlay")).toHaveCount(0);
  await page.keyboard.press("F4");
  expect(voiceRequests).toEqual([]);
  await page.screenshot({ path: testInfo.outputPath("frontend-pending.png") });
  snapshot.resolve();
  await expect(page.getByTestId("voice-overlay")).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("frontend-ready.png") });
});
