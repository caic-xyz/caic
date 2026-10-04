// Browser coverage for task-container file links, image display, text, and missing artifacts.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`task file links open artifacts at ${width}px`, async ({ page, api, request, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const id = await createTaskAPI(
      api,
      uniquePrompt(
        "Review [Screenshot](/home/user/src/caic/test-results/screenshot.png) and [Readme](/home/user/src/caic/README.md).",
      ),
    );
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    const link = page.getByRole("link", { name: "Screenshot", exact: true });
    await expect(link).toBeVisible();
    const opened = page.waitForEvent("popup");
    await link.click();
    const imagePage = await opened;
    await imagePage.waitForLoadState();
    const image = imagePage.locator("img");
    await expect(image).toBeVisible();
    expect(await image.evaluate((el: HTMLImageElement) => el.naturalWidth)).toBe(1);
    await imagePage.close();
    await expect(page).toHaveURL(`/task/@${id}`);

    const textOpened = page.waitForEvent("popup");
    await page.getByRole("link", { name: "Readme", exact: true }).click();
    const textPage = await textOpened;
    await expect(textPage.locator("body")).toContainText("# Task artifact");
    await textPage.close();

    const fileURL = `/api/caic/v1/tasks/${id}/file?path=%2Fhome%2Fuser%2Fsrc%2Fcaic%2Ftest-results%2Fscreenshot.png`;
    const partial = await request.get(fileURL, {
      headers: { Range: "bytes=8-11", "Accept-Encoding": "gzip" },
    });
    expect(partial.status()).toBe(206);
    expect(partial.headers()["content-type"]).toBe("image/png");
    expect(partial.headers()["content-range"]).toMatch(/^bytes 8-11\/\d+$/);
    expect((await partial.body()).byteLength).toBe(4);
    expect(partial.headers()["cache-control"]).toBe("no-store");
    expect(partial.headers()["content-encoding"]).toBe("identity");
    const fresh = await request.get(fileURL, { headers: { "If-None-Match": "*" } });
    expect(fresh.status()).toBe(200);
    expect(fresh.headers()["cache-control"]).toBe("no-store");
    expect(fresh.headers()["etag"]).toBeUndefined();
    expect(fresh.headers()["last-modified"]).toBeUndefined();

    const missing = await request.get(`/api/caic/v1/tasks/${id}/file?path=%2Fmissing.png`);
    expect(missing.status()).toBe(404);
    expect(await missing.json()).toMatchObject({ error: { code: "NOT_FOUND", message: "file not found" } });
  });
}
