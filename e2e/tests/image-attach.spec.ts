// E2E tests for image attachment: API image support and screenshot capture UI flow.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

// Minimal 1×1 transparent PNG encoded as base64, used as a lightweight test fixture
// when we need valid image bytes to send via the API.
const TINY_PNG_B64 =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVQI12NgAAIABQAABjE+ibYAAAAASUVORK5CYII=";

test("API: images are accepted in task inputs when harness supports them", async ({ api }) => {
  const id = await createTaskAPI(api, `image-api ${Date.now()}`);
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

  const prompt = `screenshot-ui ${Date.now()}`;
  const id = await createTaskAPI(api, prompt);
  await waitForTaskState(api, id, "waiting");

  await page.goto("/");
  await page.getByText(prompt).first().click();

  // Wait for the agent's first response before touching the input.
  await expect(
    page.getByText("Why do programmers prefer dark mode?").first(),
  ).toBeVisible({ timeout: 15_000 });

  // Scope all input interactions to the task-detail form to avoid ambiguity
  // with the sidebar's prompt-input which also has an "Attach images" button.
  const detailForm = page
    .locator("form")
    .filter({ has: page.getByPlaceholder("Send message to agent...") });

  // Open the attach menu and trigger screenshot capture.
  await detailForm.getByTitle("Attach images").click();
  await page.getByRole("menuitem", { name: "Screenshot" }).click();

  // A thumbnail (img[alt="attached"]) must appear in the input preview strip.
  // Scope to detailForm so we don't match images in the conversation history,
  // which renders sent images with the same alt="attached" attribute.
  const thumbnail = detailForm.getByRole("img", { name: "attached" });
  await expect(thumbnail).toBeVisible({ timeout: 5_000 });

  // Send the screenshot together with a text message.
  await detailForm.getByPlaceholder("Send message to agent...").fill("here is a screenshot");
  await detailForm.getByTitle("Send").click();

  // After a successful send the input images are cleared, so the preview-strip
  // thumbnail disappears (the image still appears in the conversation history).
  await expect(thumbnail).not.toBeVisible({ timeout: 5_000 });

  // Agent processes the turn and returns to "waiting".
  await waitForTaskState(api, id, "waiting");

  await api.purgeTask(id);
  await waitForTaskState(api, id, "purged");
});
