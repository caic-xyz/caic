// Browser coverage for compressed STEP links, geometry import, and archive errors.
import { readFileSync } from "node:fs";
import { gzipSync } from "node:zlib";
import { zipSync } from "three/addons/libs/fflate.module.js";
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`compressed STEP models open at ${width}px`, async ({ page, context, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const step = readFileSync("node_modules/occt-import-js/test/testfiles/simple-basic-cube/cube.stp");
    const zip = Buffer.from(zipSync({ "models/cube.stp": step }));
    const stpz = Buffer.from(zipSync({ "ISO-10303.p21": step }));
    await context.route("**/raw?*", async (route) => {
      const path = new URL(route.request().url()).searchParams.get("path");
      if (path === "/cube.step") {
        await route.fulfill({ contentType: "application/octet-stream", body: step });
      } else if (path === "/cube.stpz") {
        await route.fulfill({ contentType: "application/zip", body: stpz });
      } else if (path === "/cube.step.zip") {
        await route.fulfill({ contentType: "application/zip", body: zip });
      } else if (path === "/cube.stp.gz") {
        await route.fulfill({
          contentType: "application/gzip",
          body: Buffer.concat([gzipSync(step.subarray(0, 4000)), gzipSync(step.subarray(4000))]),
        });
      } else if (path === "/broken.stpz") {
        await route.fulfill({ contentType: "application/octet-stream", body: "invalid archive" });
      } else {
        await route.continue();
      }
    });
    const id = await createTaskAPI(
      api,
      uniquePrompt(
        "Review [Plain](/cube.step), [STPZ](/cube.stpz), [ZIP](/cube.step.zip), [Gzip](/cube.stp.gz), and [Broken](/broken.stpz).",
      ),
    );
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    for (const label of ["Plain", "STPZ", "ZIP", "Gzip"]) {
      await page.getByRole("link", { name: label, exact: true }).click();
      await expect(page.getByText("12 triangles", { exact: true })).toBeVisible();
      await expect(page.getByRole("alert")).toHaveCount(0);
      await page.keyboard.press("Escape");
      await expect(page).toHaveURL(new RegExp(`/task/@${id}(?:\\+[^/?]+)?$`));
    }
    await page.getByRole("link", { name: "Broken", exact: true }).click();
    await expect(page.getByRole("alert")).toContainText("Invalid compressed STEP file");
    expect(errors).toEqual([]);
  });
}
