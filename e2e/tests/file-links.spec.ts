// Browser coverage for unified container-file viewing, raw streams, CAD, PDFs, safe text, images, and missing files.
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";
import { createPaintedBambu3mfFixture } from "../../frontend/tests/cad-fixture";
import { createPdfFixture } from "../../frontend/tests/pdf-fixture";

for (const width of [390, 1280]) {
  test(`container files use viewer and raw handlers at ${width}px`, async ({ page, api, request, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    const requests: string[] = [];
    let holdPdf = false;
    let pdfRequests = 0;
    let resumePdf: (() => void) | undefined;
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("request", (request) => requests.push(request.url()));
    await page.route("**/raw?*", async (route) => {
      if (new URL(route.request().url()).searchParams.get("path") === "/models/assembly.3mf") {
        await route.fulfill({ contentType: "model/3mf", body: Buffer.from(createPaintedBambu3mfFixture("8")) });
      } else if (new URL(route.request().url()).searchParams.get("path") === "/notes.go") {
        await route.fulfill({ contentType: "text/plain", body: "package notes\n// # Not a Markdown heading" });
      } else if (new URL(route.request().url()).searchParams.get("path") === "/notes.md") {
        await route.fulfill({
          contentType: "text/html",
          body: '# Notes\n\n<img src="/missing-preview.png" onerror="window.fileScriptExecuted=true"><script>window.fileScriptExecuted=true</script><style>body{display:none}</style><form method="post" action="/api/caic/v1/tasks"><button style="position:fixed;inset:0;opacity:0" formaction="/api/caic/v1/tasks/abc/purge" form="target">Submit</button></form>\n\n[Run](javascript:alert%281%29)',
        });
      } else if (new URL(route.request().url()).searchParams.get("path") === "/report.bin") {
        await route.fulfill({ contentType: "application/octet-stream", body: Buffer.from([0, 1, 2]) });
      } else if (new URL(route.request().url()).searchParams.get("path") === "/report.pdf") {
        pdfRequests++;
        if (holdPdf && pdfRequests === 2)
          await new Promise<void>((resolve) => {
            resumePdf = resolve;
          });
        await route.fulfill({ contentType: "application/pdf", body: createPdfFixture() });
      } else if (new URL(route.request().url()).searchParams.get("path") === "/broken.pdf") {
        await route.fulfill({ contentType: "application/pdf", body: "%PDF-1.7\ninvalid" });
      } else {
        await route.continue();
      }
    });
    const id = await createTaskAPI(
      api,
      uniquePrompt(
        "Review [Screenshot](/home/user/src/caic/test-results/screenshot.png), [Readme](/home/user/src/caic/README.md), [Bracket](/home/user/src/caic/models/bracket.stl), [Print](/models/assembly.3mf), [Source](/notes.go), [Binary](/report.bin), [PDF](/report.pdf), [Broken PDF](/broken.pdf), and [Notes](/notes.md).",
      ),
    );
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    const link = page.getByRole("link", { name: "Screenshot", exact: true });
    await expect(link).toBeVisible();
    await link.click();
    await expect(page).toHaveURL(
      `/task/@${id}/view?path=${encodeURIComponent("/home/user/src/caic/test-results/screenshot.png")}`,
    );
    const image = page.getByRole("img", { name: "/home/user/src/caic/test-results/screenshot.png", exact: true });
    await expect(image).toBeVisible();
    expect(await image.evaluate((el: HTMLImageElement) => el.naturalWidth)).toBe(1);
    await page.screenshot({ path: test.info().outputPath("image-viewer.png") });
    const opened = page.waitForEvent("popup");
    await page.getByRole("link", { name: "raw file" }).click();
    const rawPage = await opened;
    await expect(rawPage.locator("img")).toBeVisible();
    await rawPage.close();
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(new RegExp(`/task/@${id}\\b`));

    await test.step("source and unsupported files open the same viewer", async () => {
      await page.getByRole("link", { name: "Source", exact: true }).click();
      await expect(page.getByTestId("document-pane").locator("pre")).toHaveText(
        "package notes\n// # Not a Markdown heading",
      );
      await page.keyboard.press("Escape");
      await page.getByRole("link", { name: "Binary", exact: true }).click();
      await expect(page.getByText(/No preview for this file/)).toBeVisible();
      await expect(page.getByRole("link", { name: "raw file" })).toHaveAttribute(
        "href",
        `/api/caic/v1/tasks/${id}/raw?path=%2Freport.bin`,
      );
      await page.keyboard.press("Escape");
    });

    await test.step("PDF pages render with navigation, zoom, history, and failure feedback", async () => {
      expect(requests.some((url) => /\/assets\/(PdfPreview|pdf\.worker)/.test(url))).toBe(false);
      await page.getByRole("link", { name: "PDF", exact: true }).click();
      const preview = page.getByRole("region", { name: "PDF page", exact: true });
      await expect(preview).toHaveAttribute("aria-busy", "false");
      await expect(preview).toContainText("First PDF page");
      await preview.getByText("First PDF page", { exact: true }).click({ clickCount: 3 });
      expect(await page.evaluate(() => window.getSelection()?.toString())).toContain("First PDF page");
      await expect(page.getByRole("button", { name: "Previous page", exact: true })).toBeDisabled();
      const canvas = preview.locator("canvas");
      expect(
        await canvas.evaluate((el: HTMLCanvasElement) => {
          const context = el.getContext("2d");
          return (
            context &&
            Array.from(context.getImageData(0, 0, el.width, el.height).data).some(
              (value, index) => index % 4 !== 3 && value < 100,
            )
          );
        }),
      ).toBe(true);
      await page.screenshot({ path: test.info().outputPath("pdf-viewer.png") });
      const dimensions = await preview.evaluate((el) => ({
        height: el.clientHeight,
        pageHeight: el.querySelector("canvas")?.clientHeight ?? 0,
        pageWidth: el.querySelector("canvas")?.clientWidth ?? 0,
      }));
      // Resize through the band where fit-width pages gain/lose a scrollbar.
      for (const delta of [-5, 5]) {
        await page.setViewportSize({ width, height: 900 + dimensions.pageHeight - dimensions.height + delta });
        await expect(preview).toHaveAttribute("aria-busy", "false");
        await expect(preview.locator("canvas")).toHaveJSProperty("clientWidth", dimensions.pageWidth);
      }
      await page.setViewportSize({ width, height: 900 });
      const nextPage = page.getByRole("button", { name: "Next page", exact: true });
      await nextPage.focus();
      await page.keyboard.press("Enter");
      await expect(preview).toHaveAttribute("aria-busy", "false");
      await expect(preview).toContainText("Second PDF page");
      await expect(nextPage).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(page.getByText("Page 2 of 2", { exact: true })).toBeVisible();
      const rotatedText = preview.getByText("Second PDF page", { exact: true });
      const textBounds = await rotatedText.boundingBox();
      const canvasBounds = await preview.locator("canvas").boundingBox();
      if (!textBounds || !canvasBounds) throw new Error("Missing PDF page bounds");
      expect(textBounds.height).toBeGreaterThan(textBounds.width);
      expect(textBounds.x).toBeGreaterThanOrEqual(canvasBounds.x);
      expect(textBounds.y).toBeGreaterThanOrEqual(canvasBounds.y);
      expect(textBounds.x + textBounds.width).toBeLessThanOrEqual(canvasBounds.x + canvasBounds.width);
      expect(textBounds.y + textBounds.height).toBeLessThanOrEqual(canvasBounds.y + canvasBounds.height);
      await rotatedText.click({ clickCount: 3 });
      expect(await page.evaluate(() => window.getSelection()?.toString())).toContain("Second PDF page");
      await expect(page).toHaveURL(/pdfPage=2/);
      await expect(page.getByRole("button", { name: "Next page", exact: true })).toBeDisabled();
      await page.reload();
      await expect(preview).toContainText("Second PDF page");
      await page.getByRole("combobox", { name: "PDF zoom" }).selectOption("2");
      await expect(preview).toHaveAttribute("aria-busy", "false");
      await expect(preview.locator("canvas")).toHaveJSProperty("clientWidth", 800);
      await page.goBack();
      await expect(page.getByRole("combobox", { name: "PDF zoom" })).toHaveValue("fit");
      await page.goBack();
      await expect(preview).toContainText("First PDF page");
      await page.getByRole("button", { name: "Full screen", exact: true }).click();
      await expect(preview).toHaveAttribute("aria-busy", "false");
      const contained = await preview.locator("canvas").evaluate((el) => {
        const bounds = el.getBoundingClientRect();
        return bounds.right <= window.innerWidth;
      });
      expect(contained).toBe(true);
      await page.keyboard.press("Escape");
      await page.keyboard.press("Escape");
      await page.getByRole("link", { name: "Broken PDF", exact: true }).click();
      await expect(page.getByRole("alert")).toContainText("Invalid PDF");
      await expect(page.getByRole("link", { name: "raw file" })).toHaveAttribute(
        "href",
        `/api/caic/v1/tasks/${id}/raw?path=%2Fbroken.pdf`,
      );
      await page.keyboard.press("Escape");
      expect(errors).toEqual([]);
    });

    await test.step("closing a PDF cancels a pending load", async () => {
      holdPdf = true;
      pdfRequests = 0;
      await page.getByRole("link", { name: "PDF", exact: true }).click();
      await expect.poll(() => Boolean(resumePdf)).toBe(true);
      await page.keyboard.press("Escape");
      resumePdf?.();
      await page.getByRole("link", { name: "Source", exact: true }).click();
      await expect(page.getByTestId("document-pane").locator("pre")).toContainText("package notes");
      await expect(page.getByRole("region", { name: "PDF page", exact: true })).toHaveCount(0);
      expect(errors).toEqual([]);
      await page.keyboard.press("Escape");
    });

    await test.step("Markdown content cannot execute scripts", async () => {
      await page.getByRole("link", { name: "Notes", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Notes" })).toBeVisible();
      await expect(
        page.locator("article").locator("[onerror], form, [form], [formaction], a[href^='javascript:']"),
      ).toHaveCount(0);
      await page.waitForFunction(
        () => document.querySelector<HTMLImageElement>('img[src="/missing-preview.png"]')?.complete,
      );
      expect(await page.evaluate(() => Reflect.get(window, "fileScriptExecuted"))).toBeUndefined();
      const contained = await page.getByRole("button", { name: "Submit", exact: true }).evaluate((button) => {
        const bounds = button.getBoundingClientRect();
        const documentBounds = button.closest("[data-markdown-wrap]")?.getBoundingClientRect();
        return (
          documentBounds !== undefined &&
          bounds.top >= documentBounds.top &&
          bounds.left >= documentBounds.left &&
          bounds.right <= documentBounds.right &&
          bounds.bottom <= documentBounds.bottom
        );
      });
      expect(contained).toBe(true);
      await page.keyboard.press("Escape");
    });

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
      await page.keyboard.press("Escape");
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

    await test.step("3MF links render colored component instances with viewer controls", async () => {
      await page.getByRole("link", { name: "Print", exact: true }).click();
      await expect(page).toHaveURL(`/task/@${id}/view?path=${encodeURIComponent("/models/assembly.3mf")}`);
      await expect(page.getByText("4 triangles")).toHaveAttribute("title", "Dimensions: 70 × 20 × 0");
      await expect(page.locator("canvas")).toBeVisible();
      await page.screenshot({ path: test.info().outputPath("printing-plates.png") });
      const canvas = await page.locator("canvas").boundingBox();
      if (!canvas) throw new Error("Missing canvas bounds");
      await page.mouse.move(canvas.x + canvas.width / 2, canvas.y + canvas.height / 2);
      await page.mouse.down();
      await page.mouse.move(canvas.x + canvas.width / 2 + 40, canvas.y + canvas.height / 2, { steps: 8 });
      await page.mouse.up();
      await page.screenshot({ path: test.info().outputPath("printing-plates-orbit.png") });
      await page.getByRole("button", { name: "View controls", exact: true }).focus();
      await page.getByRole("button", { name: "Reset view", exact: true }).click();
      await page.getByRole("button", { name: /^Display mode:/ }).click();
      await page.getByRole("button", { name: "View controls", exact: true }).focus();
      await page.getByRole("button", { name: "Reset view", exact: true }).click();
      await page.keyboard.press("Escape");
      await expect(page).toHaveURL(new RegExp(`/task/@${id}\\b`));
      expect(errors).toEqual([]);
    });

    const readme = await request.get(`/api/caic/v1/tasks/${id}/raw?path=%2Fhome%2Fuser%2Fsrc%2Fcaic%2FREADME.md`);
    expect(await readme.text()).toContain("# Task artifact");

    const stl = await request.get(
      `/api/caic/v1/tasks/${id}/raw?path=%2Fhome%2Fuser%2Fsrc%2Fcaic%2Fmodels%2Fbracket.stl`,
    );
    expect(stl.status()).toBe(200);
    expect(await stl.text()).toContain("solid bracket");

    const fileURL = `/api/caic/v1/tasks/${id}/raw?path=%2Fhome%2Fuser%2Fsrc%2Fcaic%2Ftest-results%2Fscreenshot.png`;
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

    const missing = await request.get(`/api/caic/v1/tasks/${id}/raw?path=%2Fmissing.png`);
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
