// E2E tests for image drafts: API compatibility, captures, limits, retry, task switching, and preview cleanup.
import { validateTask } from "../../sdk/caic/ts/v1/validate.gen";
import { test, expect, createTaskAPI, waitForTaskState, fillContentEditable } from "../helpers";

// Minimal 1×1 transparent PNG encoded as base64, used as a lightweight test fixture
// when we need valid image bytes to send via the API.
const TINY_PNG_B64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVQI12NgAAIABQAABjE+ibYAAAAASUVORK5CYII=";

test("API: images are accepted in task inputs when harness supports them", async ({ api, uniquePrompt }) => {
  const id = await createTaskAPI(api, uniquePrompt("image-api"));
  await waitForTaskState(api, id, "waiting");

  // Send a follow-up input that includes an attached image; the fake backend's
  // harness has supportsImages: true so this must be accepted (HTTP 200).
  await api.sendInput(id, {
    prompt: {
      text: "with image",
      images: [{ mediaType: "image/png", data: TINY_PNG_B64 }],
    },
  });
  await waitForTaskState(api, id, "waiting");

  await api.purgeTask(id);
  await waitForTaskState(api, id, "purged");
});

test("UI: screenshot capture attaches a thumbnail which is sent and cleared on submit", async ({
  page,
  api,
  uniquePrompt,
}) => {
  // Install a getDisplayMedia mock before the page loads.
  //
  // In headless Chromium, video.play() on a canvas captureStream() never
  // resolves — the browser doesn't generate frames without a visible compositor.
  // To work around this we:
  //   1. Draw a colored square on a source canvas.
  //   2. Override getDisplayMedia to return a captureStream from that canvas.
  //   3. Track mock streams in a WeakSet and override HTMLVideoElement.play so
  //      that, for those streams only, play() resolves immediately and the
  //      video dimensions are set to 100×100 without waiting for real playback.
  //      captureScreen() will then draw a valid (blue) 100×100 JPEG.
  await page.addInitScript(() => {
    const mockStreams: WeakSet<MediaStream> = new WeakSet();

    const srcCanvas = document.createElement("canvas");
    srcCanvas.width = 100;
    srcCanvas.height = 100;
    const ctx = srcCanvas.getContext("2d");
    if (!ctx) throw new Error("2d canvas context unavailable");
    ctx.fillStyle = "#4a90d9";
    ctx.fillRect(0, 0, 100, 100);

    Object.defineProperty(navigator.mediaDevices, "getDisplayMedia", {
      writable: true,
      configurable: true,
      value: async () => {
        const stream = srcCanvas.captureStream(30);
        mockStreams.add(stream);
        return stream;
      },
    });

    const origPlay = HTMLVideoElement.prototype.play;
    HTMLVideoElement.prototype.play = async function () {
      if (this.srcObject instanceof MediaStream && mockStreams.has(this.srcObject)) {
        // Short-circuit: resolve immediately and expose canvas dimensions.
        Object.defineProperty(this, "videoWidth", { value: 100, configurable: true });
        Object.defineProperty(this, "videoHeight", { value: 100, configurable: true });
        return Promise.resolve();
      }
      return origPlay.call(this);
    };
  });

  const prompt = uniquePrompt("screenshot-ui");
  const id = await createTaskAPI(api, prompt);
  await waitForTaskState(api, id, "waiting");

  await page.goto("/");
  const taskCard = page.locator(`[data-task-id="${id}"]`);
  await expect(taskCard).toBeVisible({ timeout: 10_000 });
  await taskCard.click();

  // Wait for the agent's first response before touching the input.
  await expect(page.getByText("Why do programmers prefer dark mode?").first()).toBeVisible({
    timeout: 15_000,
  });

  // Scope all input interactions to the task-detail form to avoid ambiguity
  // with the sidebar's prompt-input which also has an "Attach images" button.
  const detailForm = page.getByTestId("task-detail-form");

  // Open the attach menu and trigger screenshot capture.
  await detailForm.getByTestId("attach-images").click();
  await page.getByTestId("screenshot-menu-item").click();

  // A thumbnail (img[alt="attached"]) must appear in the input preview strip.
  // Scope to detailForm so we don't match images in the conversation history,
  // which renders sent images with the same alt="attached" attribute.
  const thumbnail = detailForm.getByRole("img", { name: "attached" });
  await expect(thumbnail).toBeVisible({ timeout: 5_000 });

  // Send the screenshot together with a text message.
  await fillContentEditable(
    detailForm.getByRole("textbox", { name: "Send message to agent..." }),
    "here is a screenshot",
  );
  await detailForm.getByTestId("send-input").click();

  // After a successful send the input images are cleared, so the preview-strip
  // thumbnail disappears (the image still appears in the conversation history).
  await expect(thumbnail).not.toBeVisible({ timeout: 5_000 });

  // Agent processes the turn and returns to "waiting".
  await waitForTaskState(api, id, "waiting");

  await api.purgeTask(id);
  await waitForTaskState(api, id, "purged");
});

interface DraftProbe {
  created: string[];
  revoked: string[];
  holdReads: boolean;
  readStarted: boolean;
  finishedReads: number;
  resume: (() => void) | null;
}
declare global {
  interface Window {
    draftProbe: DraftProbe;
  }
}

async function observeDrafts(page: import("@playwright/test").Page) {
  await page.addInitScript(() => {
    // Draft identities also work on LAN HTTP origins without randomUUID.
    Object.defineProperty(crypto, "randomUUID", { configurable: true, value: undefined });
    const probe: DraftProbe = {
      created: [],
      revoked: [],
      holdReads: false,
      readStarted: false,
      finishedReads: 0,
      resume: null,
    };
    window.draftProbe = probe;
    const create = URL.createObjectURL;
    const revoke = URL.revokeObjectURL;
    URL.createObjectURL = (blob) => {
      const url = create(blob);
      probe.created.push(url);
      return url;
    };
    URL.revokeObjectURL = (url) => {
      probe.revoked.push(url);
      revoke(url);
    };
    const slice = Blob.prototype.slice;
    Blob.prototype.slice = function (start, end, type) {
      const chunk = slice.call(this, start, end, type);
      if (this.type === "image/png" && probe.holdReads) {
        const read = chunk.arrayBuffer;
        chunk.arrayBuffer = async () => {
          probe.readStarted = true;
          await new Promise<void>((resolve) => {
            probe.resume = resolve;
          });
          const bytes = await read.call(chunk);
          probe.finishedReads++;
          return bytes;
        };
      }
      return chunk;
    };
  });
}

const pngFile = { name: "image.png", mimeType: "image/png", buffer: Buffer.from(TINY_PNG_B64, "base64") };

test("UI: Blob drafts reject oversized selections and preserve edits through failed and successful submission", async ({
  page,
  api,
}) => {
  await observeDrafts(page);
  await page.goto("/");
  const prompt = page.getByTestId("prompt-input");
  const form = page.locator("form").filter({ has: prompt });
  const chooser = form.locator('input[type="file"]');
  await expect(form.getByTestId("attach-images")).toBeEnabled();
  await chooser.setInputFiles({ name: "large.png", mimeType: "image/png", buffer: Buffer.alloc(10485761) });
  await expect(form.getByRole("alert")).toHaveText("Each image must be 10 MiB or smaller.");
  expect(await page.evaluate(() => window.draftProbe.created)).toEqual([]);
  await chooser.setInputFiles(pngFile);
  const thumbnail = form.getByRole("img", { name: "attached" });
  await expect(thumbnail).toHaveAttribute("src", /^blob:/);
  await fillContentEditable(prompt, "first image draft");
  await page.evaluate(() => {
    window.draftProbe.holdReads = true;
  });
  await form.getByTestId("submit-task").click();
  await page.waitForFunction(() => window.draftProbe.readStarted);
  await fillContentEditable(prompt, "edited during conversion");
  await chooser.setInputFiles({ ...pngFile, name: "second.png" });
  await expect(thumbnail).toHaveCount(2);
  const requests: import("../../sdk/caic/ts/v1/types.gen").CreateTaskReq[] = [];
  await page.route("**/api/caic/v1/tasks", async (route) => {
    if (route.request().method() !== "POST") {
      await route.continue();
      return;
    }
    requests.push(route.request().postDataJSON());
    if (requests.length === 1) {
      await route.fulfill({
        status: 400,
        json: {
          error: { code: "BAD_REQUEST", message: "Please retry this image" },
        } satisfies import("../../sdk/caic/ts/v1/types.gen").ErrorResponse,
      });
    } else await route.continue();
  });
  await page.evaluate(() => {
    window.draftProbe.holdReads = false;
    window.draftProbe.resume?.();
  });
  await expect(page.getByText("Task creation failed: Please retry this image")).toBeVisible();
  expect(requests[0].initialPrompt).toEqual({
    text: "first image draft",
    images: [{ mediaType: "image/png", data: TINY_PNG_B64 }],
  });
  expect(await page.evaluate(() => window.draftProbe.revoked)).toEqual([]);
  await expect(prompt).toHaveText("edited during conversion");
  await expect(thumbnail).toHaveCount(2);
  const createdResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/caic/v1/tasks") &&
      response.request().method() === "POST" &&
      response.status() === 200,
  );
  await form.getByTestId("submit-task").click();
  const created = validateTask(await (await createdResponse).json());
  await expect(thumbnail).toHaveCount(0);
  expect(requests[1].initialPrompt.images).toHaveLength(2);
  const urls = await page.evaluate(() => ({ created: window.draftProbe.created, revoked: window.draftProbe.revoked }));
  expect(urls.revoked).toEqual(urls.created);
  await api.purgeTask(created.id);
});

test("UI: task switching cancels conversion, retains the original draft, and releases previews on removal", async ({
  page,
  api,
  uniquePrompt,
}) => {
  const first = await createTaskAPI(api, uniquePrompt("draft-first"));
  const second = await createTaskAPI(api, uniquePrompt("draft-second"));
  await waitForTaskState(api, first, "waiting");
  await waitForTaskState(api, second, "waiting");
  await observeDrafts(page);
  await page.goto("/");
  await page.locator(`[data-task-id="${first}"]`).click();
  const form = page.getByTestId("task-detail-form");
  const thumbnail = form.getByRole("img", { name: "attached" });
  await expect(form.getByTestId("attach-images")).toBeEnabled();
  await form.locator('input[type="file"]').setInputFiles(pngFile);
  const originalURL = await thumbnail.getAttribute("src");
  await page.evaluate(() => {
    window.draftProbe.holdReads = true;
  });
  let sent = 0;
  await page.route("**/input", async (route) => {
    sent++;
    await route.continue();
  });
  await form.getByTestId("send-input").click();
  await page.waitForFunction(() => window.draftProbe.readStarted);
  await page.locator(`[data-task-id="${second}"]`).click();
  await expect(thumbnail).toHaveCount(0);
  await page.evaluate(() => {
    window.draftProbe.holdReads = false;
    window.draftProbe.resume?.();
  });
  await page.waitForFunction(() => window.draftProbe.finishedReads === 1);
  await page.locator(`[data-task-id="${first}"]`).click();
  await expect(thumbnail).toHaveAttribute("src", originalURL ?? "");
  expect(sent).toBe(0);
  expect(await page.evaluate(() => window.draftProbe.revoked)).toEqual([]);
  await form.getByRole("button", { name: "Remove", exact: true }).click();
  await expect(thumbnail).toHaveCount(0);
  expect(await page.evaluate(() => window.draftProbe.revoked)).toEqual([originalURL]);
  await api.purgeTask(first);
  await api.purgeTask(second);
});

test("UI: unavailable attachment policy keeps text-only task creation working", async ({ page, api, uniquePrompt }) => {
  await page.route("**/server/config", (route) =>
    route.fulfill({
      status: 503,
      json: {
        error: { code: "SERVER_ERROR", message: "Configuration unavailable" },
      } satisfies import("../../sdk/caic/ts/v1/types.gen").ErrorResponse,
    }),
  );
  await page.goto("/");
  const form = page.locator("form").filter({ has: page.getByTestId("prompt-input") });
  await expect(form.getByTestId("attach-images")).toBeDisabled();
  await expect(form.getByRole("status")).toHaveText("Image attachments are unavailable until server limits load.");
  await fillContentEditable(page.getByTestId("prompt-input"), uniquePrompt("text-only-no-image-policy"));
  const createdResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/caic/v1/tasks") &&
      response.request().method() === "POST" &&
      response.status() === 200,
  );
  await form.getByTestId("submit-task").click();
  const created = validateTask(await (await createdResponse).json());
  await expect(page.getByTestId("task-detail-form")).toBeVisible();
  await api.purgeTask(created.id);
});
