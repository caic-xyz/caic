// Browser coverage for fitted document images, actual-size zoom, scrolling, and keyboard access.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  for (const source of ["file", "attachment"]) {
    test(`${source} images fit the pane and zoom at ${width}px`, async ({ page, api, uniquePrompt }) => {
      await page.setViewportSize({ width, height: 900 });
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      const id = await createTaskAPI(api, uniquePrompt("image sizing"));
      await waitForTaskState(api, id, "waiting");
      const hash = "a".repeat(64);
      for (const [name, imageWidth, imageHeight] of [
        ["portrait", 600, 1800],
        ["landscape", 1800, 600],
        ["small", 80, 60],
      ] as const) {
        const images = await page.evaluate(
          ({ imageWidth, imageHeight }) => {
            const canvas = document.createElement("canvas");
            canvas.width = imageWidth;
            canvas.height = imageHeight;
            const ctx = canvas.getContext("2d")!;
            const gradient = ctx.createLinearGradient(0, 0, imageWidth, imageHeight);
            gradient.addColorStop(0, "#1e40af");
            gradient.addColorStop(1, "#fbbf24");
            ctx.fillStyle = gradient;
            ctx.fillRect(0, 0, imageWidth, imageHeight);
            const full = canvas.toDataURL("image/png").split(",")[1]!;
            const preview = document.createElement("canvas");
            preview.width = Math.min(imageWidth, 1280);
            preview.height = Math.round((imageHeight * preview.width) / imageWidth);
            preview.getContext("2d")!.drawImage(canvas, 0, 0, preview.width, preview.height);
            return { full, pane: preview.toDataURL("image/png").split(",")[1]! };
          },
          { imageWidth, imageHeight },
        );
        const pattern = source === "file" ? "**/raw?*" : "**/attachments/**";
        await page.route(pattern, (route) =>
          route.fulfill({
            contentType: "image/png",
            body: Buffer.from(
              new URL(route.request().url()).searchParams.has("w") ? images.pane : images.full,
              "base64",
            ),
          }),
        );
        const query = source === "file" ? `path=%2F${name}.png` : `attachment=${hash}`;
        await page.goto(`/task/@${id}/view?${query}`);
        const pane = page.getByTestId("document-pane");
        const image = pane.getByRole("img");
        await expect(image).toBeVisible();
        await expect
          .poll(() => image.evaluate((el: HTMLImageElement) => el.naturalWidth))
          .toBe(source === "attachment" ? Math.min(imageWidth, 1280) : imageWidth);
        const fitted = await image.boundingBox();
        const bounds = await pane.boundingBox();
        if (!fitted || !bounds) throw new Error("Missing image bounds");
        expect(fitted.height).toBeLessThanOrEqual(bounds.height - 28);
        expect(fitted.width).toBeLessThanOrEqual(bounds.width);
        expect(fitted.y + fitted.height).toBeLessThanOrEqual(bounds.y + bounds.height + 1);
        expect(fitted.width / fitted.height).toBeCloseTo(imageWidth / imageHeight, 2);
        expect(fitted.width).toBeLessThanOrEqual(imageWidth);
        if (name === "small") {
          expect(fitted.width).toBe(imageWidth);
          expect(fitted.height).toBe(imageHeight);
        }
        if (name === "portrait") {
          await page.screenshot({ path: test.info().outputPath("portrait-fit.png") });
          await page.setViewportSize({ width, height: 700 });
          await expect.poll(() => image.evaluate((el) => el.clientHeight)).toBeLessThan(fitted.height - 100);
          await pane.getByRole("button", { name: "Full screen", exact: true }).click();
          const fullBounds = await pane.boundingBox();
          const fullImage = await image.boundingBox();
          if (!fullBounds || !fullImage) throw new Error("Missing full-screen bounds");
          expect(fullImage.y + fullImage.height).toBeLessThanOrEqual(fullBounds.y + fullBounds.height + 1);
          await pane.getByRole("button", { name: "Exit full screen", exact: true }).click();
          await page.setViewportSize({ width, height: 900 });
          await expect(image).toHaveJSProperty("clientHeight", Math.round(fitted.height));
        }
        await pane.getByRole("button", { name: "Zoom image to actual size", exact: true }).click();
        if (source === "attachment")
          await expect(image).toHaveAttribute("src", `/api/caic/v1/tasks/${id}/attachments/${hash}`);
        await expect(image).toHaveJSProperty("clientWidth", imageWidth);
        await expect(image).toHaveJSProperty("clientHeight", imageHeight);
        // Scroll the image's viewport, keeping overflow inside the document pane.
        const viewport = image.locator("../..");
        await viewport.evaluate((el) => {
          el.scrollTop = el.scrollHeight;
          el.scrollLeft = el.scrollWidth;
        });
        if (name === "portrait") expect(await viewport.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
        if (name === "landscape") expect(await viewport.evaluate((el) => el.scrollLeft)).toBeGreaterThan(0);
        if (name === "portrait") await page.screenshot({ path: test.info().outputPath("portrait-zoom-scrolled.png") });
        await pane.getByRole("button", { name: "Fit image to pane", exact: true }).focus();
        await page.keyboard.press("Enter");
        await expect(image).toHaveJSProperty("clientHeight", Math.round(fitted.height));
        await expect(pane.getByRole("button", { name: "Zoom image to actual size", exact: true })).toBeFocused();
        await page.unroute(pattern);
      }
      expect(errors).toEqual([]);
    });
  }
}
