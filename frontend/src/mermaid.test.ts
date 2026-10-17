// Tests Pele diagram compatibility, security limits, cached accessibility, streaming replacement, and source toggling.

import { beforeEach, describe, it } from "node:test";
import { expect } from "@tests/expect";

import {
  clearMermaidCache,
  getMermaidCachedSvg,
  renderMermaidBlocks,
  renderMermaidDiagram,
  toggleMermaidView,
} from "./mermaid";

describe("mermaid", () => {
  beforeEach(() => {
    clearMermaidCache();
  });

  it("renders valid diagrams to SVG strings", async () => {
    const svg = await renderMermaidDiagram("graph TD;\nA-->B;");
    expect(svg).not.toBeNull();
    expect(svg).toContain("<svg");
    expect(svg).toContain("</svg>");
  });

  it("returns null for invalid syntax without throwing", async () => {
    const svg = await renderMermaidDiagram("invalid diagram syntax %%%");
    expect(svg).toBeNull();
  });

  it("returns null for empty diagram text", async () => {
    expect(await renderMermaidDiagram("   ")).toBeNull();
  });

  it("caches rendered SVGs", async () => {
    const first = await renderMermaidDiagram("graph TD;\nX-->Y;");
    expect(first).not.toBeNull();

    const second = await renderMermaidDiagram("graph TD;\nX-->Y;");
    expect(second).toBe(first);
  });

  for (const [name, code] of [
    ["sequence", "sequenceDiagram\nAlice->>Bob: Hello"],
    ["class", "classDiagram\nAnimal <|-- Duck"],
    ["state", "stateDiagram-v2\n[*] --> Ready\nReady --> [*]"],
    ["architecture", "architecture-beta\nservice api(server)[API]\nservice db(database)[Database]\napi:R -- L:db"],
  ]) {
    it(`renders ${name} diagrams`, async () => {
      expect(await renderMermaidDiagram(code)).toContain("<svg");
    });
  }

  it("disables links, remote images, and HTML labels", async () => {
    const svg = await renderMermaidDiagram(`flowchart LR
      A["<img src=x onerror=alert(1)>"] --> B
      B@{ img: "https://example.com/diagram.png", label: "Remote image" }
      click A "https://example.com/"
    `);
    expect(svg).not.toBeNull();
    const container = document.createElement("div");
    container.innerHTML = svg ?? "";
    expect(container.querySelector("script, style, foreignObject, a, image, img, [href], [onerror]")).toBeNull();
    expect(svg).not.toContain("https://example.com");
  });

  it("falls back for oversized sources and expanded edge counts", async () => {
    expect(await renderMermaidDiagram("flowchart TD\n" + "A".repeat(50_000))).toBeNull();
    const edges = Array.from({ length: 501 }, (_, i) => `A --> B${i}`).join("\n");
    expect(await renderMermaidDiagram(`flowchart TD\n${edges}`)).toBeNull();
  });

  it("gives repeated cached diagrams independent accessible titles and descriptions", async () => {
    const code = "flowchart LR\naccTitle: Pipeline\naccDescr: Build and test\nA --> B";
    await renderMermaidDiagram(code);
    const container = document.createElement("div");
    container.innerHTML = Array.from(
      { length: 2 },
      () => `
      <div data-mermaid-block>
        <button data-mermaid-toggle></button>
        <div data-mermaid-diagram></div>
        <div data-mermaid-source><pre><code>${code}</code></pre></div>
      </div>`,
    ).join("");
    await renderMermaidBlocks(container);
    const ids: string[] = [];
    for (const svg of container.querySelectorAll("svg")) {
      const title = svg.querySelector("title");
      const description = svg.querySelector("desc");
      expect(title?.textContent).toBe("Pipeline");
      expect(description?.textContent).toBe("Build and test");
      expect(svg.getAttribute("aria-labelledby")).toBe(title?.id);
      expect(svg.getAttribute("aria-describedby")).toBe(description?.id);
      ids.push(title?.id ?? "", description?.id ?? "");
    }
    expect(ids).toHaveLength(4);
    expect(new Set(ids).size).toBe(4);
  });

  it("ignores blocks replaced while a diagram loads", async () => {
    const container = document.createElement("div");
    container.innerHTML = `<div data-mermaid-block>
      <button data-mermaid-toggle></button>
      <div data-mermaid-diagram></div>
      <div data-mermaid-source><pre><code>flowchart LR\nOld --> Drawing</code></pre></div>
    </div>`;
    const oldDiagram = container.querySelector("[data-mermaid-diagram]");
    const pending = renderMermaidBlocks(container);
    container.replaceChildren();
    await pending;
    expect(oldDiagram?.innerHTML).toBe("");
  });

  it("renders diagram into data-mermaid-block container", async () => {
    const container = document.createElement("div");
    container.innerHTML = `
      <div data-mermaid-block>
        <div class="toolbar">
          <button type="button" data-mermaid-toggle style="display: none;">code</button>
        </div>
        <div data-mermaid-diagram style="display: none;"></div>
        <div data-mermaid-source>
          <pre><code>graph TD;\nNodeA-->NodeB;</code></pre>
        </div>
      </div>
    `;

    await renderMermaidBlocks(container);

    const block = container.querySelector<HTMLElement>("[data-mermaid-block]");
    const diagram = container.querySelector<HTMLElement>("[data-mermaid-diagram]");
    const source = container.querySelector<HTMLElement>("[data-mermaid-source]");
    const toggleBtn = container.querySelector<HTMLButtonElement>("[data-mermaid-toggle]");
    expect(block).not.toBeNull();
    expect(diagram).not.toBeNull();
    expect(source).not.toBeNull();
    expect(toggleBtn).not.toBeNull();
    if (!block || !diagram || !source || !toggleBtn) return;

    expect(diagram.innerHTML).toContain("<svg");
    expect(diagram.style.display).toBe("");
    expect(source.style.display).toBe("none");
    expect(toggleBtn.style.display).toBe("");
    expect(toggleBtn.textContent).toBe("code");

    // Toggle to source code
    toggleMermaidView(toggleBtn);
    expect(diagram.style.display).toBe("none");
    expect(source.style.display).toBe("");
    expect(toggleBtn.textContent).toBe("diagram");
    expect(block.dataset.view).toBe("code");

    // Toggle back to diagram
    toggleMermaidView(toggleBtn);
    expect(diagram.style.display).toBe("");
    expect(source.style.display).toBe("none");
    expect(toggleBtn.textContent).toBe("code");
    expect(block.dataset.view).toBe("diagram");
  });

  it("keeps source visible when diagram syntax is invalid", async () => {
    const container = document.createElement("div");
    container.innerHTML = `
      <div data-mermaid-block>
        <button type="button" data-mermaid-toggle style="display: none;">code</button>
        <div data-mermaid-diagram style="display: none;"></div>
        <div data-mermaid-source>
          <pre><code>graph TD;\nIncomplete -</code></pre>
        </div>
      </div>
    `;

    await renderMermaidBlocks(container);

    const diagram = container.querySelector<HTMLElement>("[data-mermaid-diagram]");
    const source = container.querySelector<HTMLElement>("[data-mermaid-source]");
    const toggleBtn = container.querySelector<HTMLButtonElement>("[data-mermaid-toggle]");
    expect(diagram).not.toBeNull();
    expect(source).not.toBeNull();
    expect(toggleBtn).not.toBeNull();
    if (!diagram || !source || !toggleBtn) return;

    expect(diagram.innerHTML).toBe("");
    expect(diagram.style.display).toBe("none");
    expect(source.style.display).toBe("");
    expect(toggleBtn.style.display).toBe("none");
  });

  it("returns cached SVG synchronously via getMermaidCachedSvg", async () => {
    const code = "graph TD;\nCacheA-->CacheB;";
    expect(getMermaidCachedSvg(code)).toBeNull();

    const rendered = await renderMermaidDiagram(code);
    expect(rendered).not.toBeNull();
    expect(getMermaidCachedSvg(code)).toBe(rendered);
  });

  it("preserves pre-rendered SVG in block without replacing DOM", async () => {
    const container = document.createElement("div");
    const code = "graph TD;\nPre1-->Pre2;";
    const fakeSvg = '<svg id="custom-test-svg"></svg>';
    // Pre-populate cache
    await renderMermaidDiagram(code);

    const block = document.createElement("div");
    block.dataset.mermaidBlock = "";
    block.innerHTML = `
      <div class="toolbar">
        <button type="button" data-mermaid-toggle>code</button>
      </div>
      <div data-mermaid-diagram>${fakeSvg}</div>
      <div data-mermaid-source style="display: none;">
        <pre><code>${code}</code></pre>
      </div>
    `;
    container.appendChild(block);

    // Manually prime the cache via renderMermaidDiagram
    await renderMermaidBlocks(container);

    const diagram = block.querySelector<HTMLElement>("[data-mermaid-diagram]");
    expect(diagram?.querySelector("svg")?.id).toBe("custom-test-svg");
  });
});
