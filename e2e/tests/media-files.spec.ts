// Browser coverage for document audio/video autoplay, responsive native controls, failures, and cleanup.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import { createWebmFixture } from "../../frontend/tests/media-fixture";

// Five seconds of mono PCM silence, with a standard WAV header.
function wav(): Buffer {
  const samples = 8000 * 5;
  const body = Buffer.alloc(44 + samples * 2);
  body.write("RIFF", 0);
  body.writeUInt32LE(body.length - 8, 4);
  body.write("WAVEfmt ", 8);
  body.writeUInt32LE(16, 16);
  body.writeUInt16LE(1, 20);
  body.writeUInt16LE(1, 22);
  body.writeUInt32LE(8000, 24);
  body.writeUInt32LE(16000, 28);
  body.writeUInt16LE(2, 32);
  body.writeUInt16LE(16, 34);
  body.write("data", 36);
  body.writeUInt32LE(samples * 2, 40);
  return body;
}

for (const width of [390, 1280]) {
  test(`document media autoplays and stops on navigation at ${width}px`, async ({ page, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/raw?*", async (route) => {
      const path = new URL(route.request().url()).searchParams.get("path");
      if (path === "/clip.webm" || path === "/broken.webm") {
        await route.fulfill({
          contentType: "video/webm",
          headers: { "Content-Disposition": "attachment" },
          body: path === "/clip.webm" ? createWebmFixture() : Buffer.from("invalid video"),
        });
      } else if (path === "/recording.wav" || path === "/broken.mp3") {
        await route.fulfill({
          contentType: path === "/recording.wav" ? "audio/wave" : "audio/mpeg",
          headers: { "Content-Disposition": "attachment" },
          body: path === "/recording.wav" ? wav() : Buffer.from("invalid audio"),
        });
      } else await route.continue();
    });
    const id = await createTaskAPI(
      api,
      uniquePrompt(
        "Open [Recording](/recording.wav), [Clip](/clip.webm), [Broken audio](/broken.mp3), or [Broken video](/broken.webm).",
      ),
    );
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    for (const [kind, label, path, broken] of [
      ["audio", "Recording", "/recording.wav", "/broken.mp3"],
      ["video", "Clip", "/clip.webm", "/broken.webm"],
    ] as const) {
      await test.step(`${kind} autoplays and stops on close`, async () => {
        await page.getByRole("link", { name: label, exact: true }).click();
        const media = page.getByLabel(path, { exact: true });
        await expect(media).toBeVisible();
        await expect(media).toHaveJSProperty("duration", 5);
        await expect(media).toHaveJSProperty("paused", false);
        await expect.poll(() => media.evaluate((el: HTMLMediaElement) => el.currentTime)).toBeGreaterThan(0);
        if (kind === "video") {
          await expect(media).toHaveJSProperty("videoWidth", 320);
          await expect(media).toHaveAttribute("playsinline", "");
        }
        const bounds = await media.boundingBox();
        expect(bounds).not.toBeNull();
        expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);

        const playing = await media.elementHandle();
        await page.getByRole("button", { name: "Close view", exact: true }).click();
        await expect(page).toHaveURL(new RegExp(`/task/@${id}(?:\\+[^/]+)?$`));
        expect(await playing!.evaluate((el: HTMLMediaElement) => el.paused && !el.hasAttribute("src"))).toBe(true);
      });
      await test.step(`${kind} reports decode failure`, async () => {
        await page.getByRole("link", { name: `Broken ${kind}`, exact: true }).click();
        await expect(page.getByRole("alert")).toContainText(`Unable to play this ${kind} file`);
        await expect(page.getByRole("link", { name: "raw file" })).toHaveAttribute(
          "href",
          `/api/caic/v1/tasks/${id}/raw?path=${encodeURIComponent(broken)}`,
        );
        await page.getByRole("button", { name: "Close view", exact: true }).click();
      });
    }
    expect(errors).toEqual([]);
  });
}
