// Tests Mermaid diagram compilation, syntax failure fallback, caching, and view toggling.

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
