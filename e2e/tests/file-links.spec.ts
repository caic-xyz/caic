// Browser coverage for task-container file links, image display, markdown viewer, CAD 3D viewer, inline mermaid diagrams, and missing artifacts.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`task file links open artifacts and markdown at ${width}px`, async ({ page, api, request, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const id = await createTaskAPI(
      api,
      uniquePrompt(
        "Review [Screenshot](/home/user/src/caic/test-results/screenshot.png), [Readme](/home/user/src/caic/README.md), and [Bracket](/home/user/src/caic/models/bracket.stl).",
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

    await test.step("markdown links open in the in-app viewer", async () => {
      const readmeURL = "/home/user/src/caic/README.md";
      await page.getByRole("link", { name: "Readme", exact: true }).click();
      await expect(page).toHaveURL(`/task/@${id}/view?path=${encodeURIComponent(readmeURL)}`);
      await expect(page.getByRole("heading", { name: "Task artifact" })).toBeVisible();

      await page.getByRole("link", { name: "guide", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Guide" })).toBeVisible();
      const shot = page.getByRole("img", { name: "Shot" });
      await expect(shot).toBeVisible();
      expect(await shot.evaluate((el: HTMLImageElement) => el.naturalWidth)).toBe(1);

      await page.getByRole("link", { name: "README", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Task artifact" })).toBeVisible();
      await page.getByRole("button", { name: "Back to task" }).click();
      await expect(page).toHaveURL(new RegExp(`/task/@${id}\\b`));
    });

    await test.step("cad model links open in the in-app 3d viewer", async () => {
      const bracketURL = "/home/user/src/caic/models/bracket.stl";
      await page.getByRole("link", { name: "Bracket", exact: true }).click();
      await expect(page).toHaveURL(`/task/@${id}/view?path=${encodeURIComponent(bracketURL)}`);
      await expect(page.getByTitle(bracketURL)).toBeVisible();
      await expect(page.getByRole("link", { name: "raw file" })).toBeVisible();
      await expect(page.getByText("1 triangles")).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(page).toHaveURL(new RegExp(`/task/@${id}\\b`));
    });

    const readme = await request.get(`/api/caic/v1/tasks/${id}/file?path=%2Fhome%2Fuser%2Fsrc%2Fcaic%2FREADME.md`);
    expect(await readme.text()).toContain("# Task artifact");

    const stl = await request.get(
      `/api/caic/v1/tasks/${id}/file?path=%2Fhome%2Fuser%2Fsrc%2Fcaic%2Fmodels%2Fbracket.stl`,
    );
    expect(stl.status()).toBe(200);
    expect(await stl.text()).toContain("solid bracket");

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

test("inline mermaid diagram renders in task view with toggle", async ({ page, api, uniquePrompt }) => {
  const prompt = uniquePrompt("Architecture:\n\n```mermaid\ngraph TD;\n  Client-->Server;\n```\n\nDiagram notes:");
  const id = await createTaskAPI(api, prompt);
  await waitForTaskState(api, id, "waiting");
  await page.goto(`/task/@${id}`);

  // Expand the first turn to reveal the prompt
  await page
    .getByRole("button", { name: /1 message/ })
    .first()
    .click();

  const diagram = page.getByRole("img", { name: "Mermaid diagram" });
  await expect(diagram).toBeVisible();
  await expect(diagram.locator("svg")).toBeVisible();

  const toggleBtn = page.getByRole("button", { name: "Show code" });
  await expect(toggleBtn).toBeVisible();
  await toggleBtn.click();

  await expect(diagram).toBeHidden();
  await expect(page.locator("code.language-mermaid")).toBeVisible();
  await expect(page.getByRole("button", { name: "Show diagram" })).toBeVisible();
});
