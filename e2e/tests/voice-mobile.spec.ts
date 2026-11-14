// Browser coverage for voice setup focus, choices, microphone recovery, native focus, and mobile task navigation.

import {
  createTaskAPI,
  expect,
  test,
  waitForTaskState,
  installNativeVoiceBridge,
  invokeFrontendTool,
  type Page,
} from "../helpers";
import type { Config } from "../../sdk/caic/ts/v1/types.gen";

async function enableBrowserVoice(page: Page) {
  await page.route("**/api/caic/v1/server/config", async (route) => {
    const response = await route.fetch();
    const config = (await response.json()) as Config;
    await route.fulfill({ json: { ...config, voiceGateway: { mode: "embedded" } } });
  });
  await page.route("**/.well-known/gomode.json", (route) =>
    route.fulfill({
      json: {
        apiVersion: 1,
        service: "caic",
        webShell: { toolGroups: [{ name: "tasks", endpoint: "/api/caic/v1/mcp" }], voiceGateway: { url: "/" } },
      },
    }),
  );
}

test("browser voice setup reads focus from shared conversation context", async ({ page, api }) => {
  const id = await createTaskAPI(api, "Review the voice setup focus context");
  await waitForTaskState(api, id, "waiting", 30_000);
  await page.addInitScript(() => {
    localStorage.setItem("gomode.voiceMode", "browser");
    // Use a real silent audio track, so the production meter and stream cleanup
    // run without requiring a microphone or speech service in the test runner.
    Object.defineProperty(navigator.mediaDevices, "getUserMedia", {
      value: async () => new AudioContext().createMediaStreamDestination().stream,
    });
  });
  await page.route("**/api/caic/v1/server/config", async (route) => {
    const response = await route.fetch();
    const config: Config = await response.json();
    await route.fulfill({ response, json: { ...config, voiceGateway: { mode: "embedded" } } satisfies Config });
  });
  await page.route("**/.well-known/gomode.json", async (route) => {
    const response = await route.fetch();
    const manifest = await response.json();
    manifest.webShell.voiceGateway = { url: "/" };
    await route.fulfill({ response, json: manifest });
  });
  await page.route("**/api/voicegateway/v2/voice/choices", (route) =>
    route.fulfill({
      json: {
        choices: [{ id: "test-text", label: "Test text voice", transports: ["text"] }],
        defaults: [{ transport: "text", id: "test-text" }],
      },
    }),
  );
  await page.route("**/api/voicegateway/v2/voice/text/ticket", (route) =>
    route.fulfill({ json: { ticket: "test-ticket" } }),
  );
  let conversation = "";
  await page.route("**/api/caic/v1/mcp", async (route) => {
    const request = route.request().postDataJSON();
    if (request.method !== "resources/read" || request.params.uri !== "caic://conversation/context") {
      await route.continue();
      return;
    }
    conversation = request.params._meta["openai/session"];
    expect(conversation).toMatch(/^[0-9a-f]{32}$/);
    const response = await route.fetch();
    expect(response.ok()).toBe(true);
    const body = await response.json();
    expect(body.error).toBeUndefined();
    const contents = body.result.contents[0];
    const projection = JSON.parse(contents.text);
    // Overlay explicit generic focus on the real context projection. A focus
    // tool would navigate other parallel test pages through owner-scoped SSE.
    projection.currentTask = { id, uri: `caic://tasks/${id}` };
    contents.text = JSON.stringify(projection);
    await route.fulfill({ response, json: body });
  });
  const setup = Promise.withResolvers<{ kind: string; context: { text: string } }>();
  await page.routeWebSocket("**/api/voicegateway/v2/voice/text/browser", (socket) => {
    socket.onMessage((message) => {
      const value = JSON.parse(String(message));
      if (value.kind === "session.setup") setup.resolve(value);
    });
  });
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto("/");
  const overlay = page.getByTestId("voice-overlay");
  await overlay.getByRole("button", { name: "Connect voice assistant", exact: true }).click();
  const message = await setup.promise;
  expect(message.kind).toBe("session.setup");
  // gomode sends each resource's text verbatim, separated by blank lines.
  const sections: string[] = message.context.text.split("\n\n");
  expect(sections).toContain(JSON.stringify({ currentTask: { id, uri: `caic://tasks/${id}` } }));
  expect(message.context.text.length).toBeLessThanOrEqual(4000);
  expect(conversation).not.toBe("");
  expect(errors).toEqual([]);
});

test("browser voice selects a text choice on mobile and displays a failed choice once", async ({ page }) => {
  await page.setViewportSize({ width: 393, height: 851 });
  await page.addInitScript(() => {
    localStorage.setItem(`gomode.voiceChoice.${location.origin}`, "gemini");
  });
  await enableBrowserVoice(page);
  let includeTextChoice = true;
  await page.route("**/api/voicegateway/v2/voice/choices", (route) =>
    route.fulfill({
      json: {
        choices: [
          { id: "gemini", label: "Gemini Live", transports: ["webrtc"] },
          ...(includeTextChoice
            ? [
                {
                  id: "text",
                  label: "Text model",
                  composable: true,
                  transports: ["text"],
                  speechModes: [{ input: "client", output: "client" }],
                },
              ]
            : []),
        ],
        defaults: [
          { transport: "webrtc", id: "gemini" },
          ...(includeTextChoice ? [{ transport: "text", id: "text" }] : []),
        ],
      },
    }),
  );
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto("/");
  const overlay = page.getByTestId("voice-overlay");
  await overlay.getByRole("button", { name: "Voice settings", exact: true }).click();
  await expect(overlay.getByRole("combobox", { name: "Model", exact: true })).toHaveValue("gemini");
  await overlay.getByRole("radio", { name: "Separate stages", exact: true }).check();
  const choice = overlay.getByRole("combobox", { name: "Language model", exact: true });
  await expect(choice).toBeEnabled();
  await expect(choice).toHaveValue("text");
  await overlay.getByRole("combobox", { name: "Recognition", exact: true }).selectOption("browser");
  await overlay.getByRole("combobox", { name: "Speech", exact: true }).selectOption("browser");
  await expect(overlay.getByRole("combobox", { name: "Recognition", exact: true })).toHaveValue("browser");
  await expect(overlay.getByRole("combobox", { name: "Speech", exact: true })).toHaveValue("browser");
  await page.screenshot({ path: test.info().outputPath("voice-parts-mobile.png") });
  await page.reload();
  await overlay.getByRole("button", { name: "Voice settings", exact: true }).click();
  await expect(overlay.getByRole("radio", { name: "Separate stages", exact: true })).toBeChecked();
  await expect(choice).toHaveValue("text");
  await overlay.getByRole("radio", { name: "All-in-one", exact: true }).check();
  await expect(overlay.getByRole("combobox", { name: "Model", exact: true })).toHaveValue("gemini");
  await overlay.getByRole("radio", { name: "Separate stages", exact: true }).check();
  await expect(choice).toHaveValue("text");
  await expect(overlay.getByRole("alert")).toHaveCount(0);
  await overlay.getByRole("button", { name: "Close voice settings", exact: true }).click();
  includeTextChoice = false;
  await overlay.getByRole("button", { name: "Connect voice assistant", exact: true }).click();
  const failure = overlay.getByText("The voice gateway has no model for the selected recognition and speech output", {
    exact: true,
  });
  await expect(failure.filter({ visible: true })).toHaveCount(1);
  await overlay.getByRole("button", { name: "Voice settings", exact: true }).click();
  await expect(failure.filter({ visible: true })).toHaveCount(1);
  expect(errors).toEqual([]);
});

test("browser voice explains an unavailable microphone without starting a connection", async ({ page }) => {
  await page.addInitScript(() => Object.defineProperty(navigator, "mediaDevices", { value: undefined }));
  await enableBrowserVoice(page);
  await page.route("**/api/voicegateway/v2/voice/choices", (route) =>
    route.fulfill({
      json: {
        choices: [{ id: "gemini", label: "Gemini Live", transports: ["webrtc"] }],
        defaults: [{ transport: "webrtc", id: "gemini" }],
      },
    }),
  );
  let offers = 0;
  await page.route("**/api/voicegateway/v2/voice/rtc/offer", (route) => {
    offers++;
    return route.abort();
  });
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto("/");
  const overlay = page.getByTestId("voice-overlay");
  const microphone = overlay.getByRole("button", { name: "Enable microphone", exact: true });
  await expect(microphone).toBeVisible();
  await microphone.click();
  await expect(
    overlay.getByRole("status").filter({ hasText: "Microphone requires HTTPS or a supported browser." }),
  ).toBeVisible();
  await expect(microphone).toHaveAccessibleDescription("Microphone requires HTTPS or a supported browser.");
  await expect(microphone).toBeVisible();
  expect(offers).toBe(0);
  expect(errors).toEqual([]);
});

test("mobile voice task links open details and browser back restores the voice list", async ({ page, api }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installNativeVoiceBridge(page, true);
  const id = await createTaskAPI(api, "Review mobile voice task view");
  const task = await waitForTaskState(api, id, "waiting", 30_000);

  await page.goto("/");
  const voiceView = page.getByTestId("mobile-voice-tasks");
  await expect(voiceView).toBeVisible();
  const taskLink = voiceView.locator(`[data-task-id="${id}"]`);
  await expect(taskLink).toContainText(task.title);
  await expect(taskLink).toContainText(/#\d+/);
  await expect(taskLink).toHaveAttribute("href", `/task/@${id}`);
  await expect(page.getByTestId("new-task-button")).toBeHidden();
  await expect(page.getByTestId("new-task-form")).toBeHidden();
  await expect(page.getByTestId("task-list")).toBeHidden();
  await expect.poll(() => voiceView.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

  const result = await invokeFrontendTool(page, "focus_task", { task_number: id });
  expect(result).toEqual({ task_id: id });
  await expect(page).toHaveURL(/\/$/);
  await expect(taskLink).toHaveAttribute("aria-current", "true");
  await taskLink.click();
  await expect(page).toHaveURL(new RegExp(`/task/@${id}$`));
  await expect(voiceView).not.toBeVisible();
  await expect(page.getByTestId("detail-pane")).toBeVisible();

  await page.goBack();
  await expect(voiceView).toBeVisible();

  await page.setViewportSize({ width: 900, height: 844 });
  await expect(voiceView).not.toBeVisible();
  await expect(page.getByTestId("task-list")).toBeVisible();
  await invokeFrontendTool(page, "focus_task", { task_number: id });
  await expect(page).toHaveURL(new RegExp(`/task/@${id}(?:\\+[^/]+)?$`));
  await expect(page.getByTestId("detail-pane")).toBeVisible();
});

test("six long task titles fit above voice controls on a Pixel 6", async ({ page, api }) => {
  await page.setViewportSize({ width: 393, height: 851 });
  await installNativeVoiceBridge(page, true);

  const ids = await Promise.all(
    Array.from({ length: 6 }, (_, index) =>
      createTaskAPI(
        api,
        `Review the complete implementation and carefully explain the next steps for the mobile voice task layout ${index}`,
      ),
    ),
  );
  const tasks = await Promise.all(ids.map((id) => waitForTaskState(api, id, "waiting", 30_000)));

  await page.goto("/");
  const voiceView = page.getByTestId("mobile-voice-tasks");
  const rows = voiceView.locator("li");
  await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(6);
  await expect.poll(() => rows.nth(5).evaluate((row) => row.getBoundingClientRect().bottom)).toBeLessThan(851 - 180);

  const name = voiceView.locator(`[data-task-id="${ids[0]}"]`).locator("span").nth(1);
  const titleMetrics = await name.evaluate((el) => ({
    height: el.getBoundingClientRect().height,
    lineHeight: Number.parseFloat(getComputedStyle(el).lineHeight),
    fullTitle: el.textContent,
  }));
  expect(titleMetrics.fullTitle).toBe(tasks[0]?.title);
  expect(titleMetrics.fullTitle).toHaveLength(80);
  expect(titleMetrics.height).toBeLessThanOrEqual(titleMetrics.lineHeight * 2 + 1);
});
