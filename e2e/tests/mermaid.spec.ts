// Browser coverage, optional stylesheet parity, and cold-load measurements for inline Mermaid diagrams.
import { readFileSync } from "node:fs";
import { test, expect, createTaskAPI, waitForTaskState } from "../helpers";

for (const width of [390, 1280]) {
  test(`diagrams are safe, accessible, and contained at ${width}px`, async ({ page, api, uniquePrompt }) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    const remoteRequests: string[] = [];
    const assets: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => {
      if (message.type() === "error") errors.push(message.text());
    });
    page.on("request", (request) => {
      if (request.url().startsWith("https://example.com/diagram")) remoteRequests.push(request.url());
      if (new URL(request.url()).pathname.startsWith("/assets/")) assets.push(request.url());
    });
    const flowchart =
      "flowchart LR\naccTitle: Task lifecycle\naccDescr: Agent executes in a container\nsubgraph Task\nAgent[Coding agent with a long descriptive label] --> Container[Isolated container]\nend\nstyle Container fill: NoNe";
    const diagrams = [
      flowchart,
      flowchart,
      "sequenceDiagram\nparticipant User\nparticipant Agent\nUser->>Agent: Build the feature\nAgent-->>User: Ready for review",
      "architecture-beta\nservice api(server)[API]\nservice db(database)[Database]\napi:R -- L:db",
      'flowchart LR\nA["<img src=x onerror=alert(1)>"] --> B\nB@{ img: "https://example.com/diagram.png", label: "Image disabled" }\nclick A "https://example.com/diagram-link"',
      "stateDiagram-v2\n[*] --> Running\nRunning --> Waiting: needs input\nWaiting --> Running: reply\nRunning --> [*]: done",
      "flowchart LR\nIncomplete[",
    ];
    const prompt = `${uniquePrompt("Diagrams:")}\n\n${diagrams.map((source) => `\`\`\`mermaid\n${source}\n\`\`\`\n`).join("\n")}`;
    const id = await createTaskAPI(api, prompt);
    await waitForTaskState(api, id, "waiting");
    await page.goto(`/task/@${id}`);
    expect(assets.filter((url) => /\/(lazy|flowchart|sequence|architecture)-/.test(url))).toEqual([]);
    await page
      .getByRole("button", { name: /1 message/ })
      .first()
      .click();
    const blocks = page.locator("[data-mermaid-block]");
    await expect(blocks.locator("[data-mermaid-diagram] > svg")).toHaveCount(6);
    await expect(blocks.last().locator("pre > code")).toBeVisible();
    expect(await blocks.locator("a, image, img, script, foreignObject").count()).toBe(0);
    const titleIds = await blocks.locator("svg title").evaluateAll((titles) => titles.map((title) => title.id));
    expect(titleIds).toHaveLength(2);
    expect(new Set(titleIds).size).toBe(2);
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    const toggle = blocks.first().getByRole("button", { name: "Show code" });
    await toggle.click();
    await expect(blocks.first().locator("pre > code")).toBeVisible();
    await blocks.first().getByRole("button", { name: "Show diagram" }).click();
    await expect(blocks.first().locator("svg")).toBeVisible();
    const firstSvg = blocks.first().locator("[data-mermaid-diagram] > svg");
    const shape = firstSvg.locator(".pele-node > rect").first();
    await expect(shape).toHaveCSS("filter", /drop-shadow/);
    const unfilled = firstSvg.locator('.pele-node[data-id="Container"] > rect');
    await expect(unfilled).toHaveCSS("fill", "none");
    await expect(unfilled).toHaveCSS("filter", "none");
    await expect(firstSvg).toHaveCSS("filter", "none");
    await expect(firstSvg.locator("text").first()).toHaveCSS("filter", "none");
    await expect(firstSvg.locator(".pele-edge").first()).toHaveCSS("filter", "none");

    // Optional parity check for a separately distributed stylesheet. CI does not
    // depend on an author's local skills checkout.
    const cssPath = process.env.CAIC_E2E_DIAGRAM_CSS;
    if (cssPath) {
      const parity = await firstSvg.evaluate(
        (svg, css) => {
          const inspect = (root: Element, view: Window) =>
            [root, ...root.querySelectorAll(".pele-node > *, .pele-edge, text, .pele-cluster")].map((element) => {
              const style = view.getComputedStyle(element);
              return { fill: style.fill, stroke: style.stroke, filter: style.filter, fontFamily: style.fontFamily };
            });
          const actual = inspect(svg, window);
          const frame = document.createElement("iframe");
          frame.hidden = true;
          document.body.append(frame);
          try {
            const doc = frame.contentDocument;
            const view = frame.contentWindow;
            if (!doc || !view) throw new Error("Stylesheet preview frame unavailable");
            const stylesheet = doc.createElement("style");
            stylesheet.textContent = css;
            doc.head.append(stylesheet);
            const copy = doc.importNode(svg, true);
            doc.body.append(copy);
            return { actual, expected: inspect(copy, view) };
          } finally {
            frame.remove();
          }
        },
        readFileSync(cssPath, "utf8"),
      );
      expect(parity.actual).toEqual(parity.expected);
    }

    await page.emulateMedia({ media: "print" });
    await expect(shape).toHaveCSS("filter", "none");
    await page.emulateMedia({ media: "screen", forcedColors: "active" });
    await expect(shape).toHaveCSS("filter", "none");
    await page.emulateMedia({ forcedColors: "none" });
    await expect(shape).toHaveCSS("filter", /drop-shadow/);
    await page.screenshot({ path: test.info().outputPath("diagrams.png"), fullPage: true });
    const architecture = blocks.nth(3).locator("[data-mermaid-diagram] > svg");
    await architecture.scrollIntoViewIfNeeded();
    await expect(architecture.locator("svg")).toHaveCount(2);
    await architecture.screenshot({ path: test.info().outputPath("architecture.png") });
    const state = blocks.nth(5).locator("[data-mermaid-diagram] > svg");
    await state.scrollIntoViewIfNeeded();
    await state.screenshot({ path: test.info().outputPath("state.png") });
    expect(remoteRequests).toEqual([]);
    expect(errors).toEqual([]);
  });
}

// pnpm exec playwright test --config e2e/playwright.config.ts \
//   e2e/tests/mermaid.spec.ts --grep benchmark --repeat-each=5 --workers=1
test("benchmark cold diagram loading", async ({ page, api, uniquePrompt }) => {
  const source =
    "flowchart LR\nBrowser[Browser client] --> API[Backend API]\nAPI --> Agent[Coding agent]\nAgent --> Container[Isolated container]\nContainer --> Repository[Git repository]";
  const id = await createTaskAPI(api, `${uniquePrompt("Architecture:")}\n\n\`\`\`mermaid\n${source}\n\`\`\`\n`);
  await waitForTaskState(api, id, "waiting");
  // Observe from navigation: fast renderers may finish before goto() returns.
  await page.addInitScript(() => {
    let started = false;
    const observer = new MutationObserver(() => {
      const diagram = document.querySelector("[data-mermaid-diagram]");
      if (!diagram) return;
      if (!started) {
        performance.mark("mermaid-start");
        started = true;
      }
      if (diagram.querySelector("svg")) {
        performance.measure("mermaid-render", "mermaid-start");
        observer.disconnect();
      }
    });
    observer.observe(document, { childList: true, subtree: true });
  });
  await page.goto(`/task/@${id}`);
  await page
    .getByRole("button", { name: /1 message/ })
    .first()
    .click();
  await expect(page.getByRole("img", { name: "Mermaid diagram" }).locator("svg")).toBeVisible();
  const result = await page.evaluate(() => {
    const assets = performance
      .getEntriesByType("resource")
      .filter((entry): entry is PerformanceResourceTiming => entry instanceof PerformanceResourceTiming)
      .filter((entry) => new URL(entry.name).pathname.startsWith("/assets/"));
    return {
      loadMs: performance.getEntriesByName("mermaid-render")[0]?.duration ?? null,
      // All page assets, including the application entry and diagram chunks.
      pageAssetBytes: assets.reduce((total, entry) => total + entry.encodedBodySize, 0),
      assets: assets.map((entry) => new URL(entry.name).pathname),
    };
  });
  expect(result.loadMs).not.toBeNull();
  console.log(JSON.stringify({ mermaid: result }));
});
