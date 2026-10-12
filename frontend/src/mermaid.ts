// Lazy-loaded Mermaid renderer: compiles diagrams to SVG with caching, responsive sizing, and code toggle.

import type mermaidApi from "mermaid";

type MermaidInstance = typeof mermaidApi;

let mermaidPromise: Promise<MermaidInstance> | null = null;
let nextDiagramId = 1;

const svgCache = new Map<string, string>();
const MAX_CACHE_SIZE = 100;

function loadMermaid(): Promise<MermaidInstance> {
  if (!mermaidPromise) {
    mermaidPromise = import("mermaid").then((m) => {
      const mermaid = m.default;
      mermaid.initialize({
        startOnLoad: false,
        theme: "neutral",
        securityLevel: "strict",
        fontFamily: "system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif",
        suppressErrorRendering: true,
      });
      return mermaid;
    });
  }
  return mermaidPromise;
}

/**
 * Returns a cached SVG string for the given Mermaid code if already compiled, or null.
 */
export function getMermaidCachedSvg(code: string): string | null {
  const trimmed = code.trim();
  if (!trimmed) return null;
  return svgCache.get(trimmed) ?? null;
}

/**
 * Validates and renders a Mermaid diagram definition to an SVG string.
 * Returns null if the diagram syntax is invalid or rendering fails.
 */
export async function renderMermaidDiagram(code: string): Promise<string | null> {
  const trimmed = code.trim();
  if (!trimmed) return null;

  const cached = svgCache.get(trimmed);
  if (cached !== undefined) return cached;

  try {
    const mermaid = await loadMermaid();
    const isValid = await mermaid.parse(trimmed, { suppressErrors: true });
    if (!isValid) return null;

    const id = `mermaid_svg_${nextDiagramId++}`;
    const { svg } = await mermaid.render(id, trimmed);

    if (svgCache.size >= MAX_CACHE_SIZE) {
      const oldest = svgCache.keys().next().value;
      if (oldest !== undefined) svgCache.delete(oldest);
    }
    svgCache.set(trimmed, svg);
    return svg;
  } catch {
    return null;
  }
}

function applyMermaidViewState(
  block: HTMLElement,
  diagramEl: HTMLElement,
  sourceEl: HTMLElement,
  toggleBtn: HTMLButtonElement,
): void {
  const isCodeView = block.dataset.view === "code";
  diagramEl.style.display = isCodeView ? "none" : "";
  sourceEl.style.display = isCodeView ? "" : "none";
  toggleBtn.style.display = "";
  toggleBtn.textContent = isCodeView ? "diagram" : "code";
  toggleBtn.title = isCodeView ? "Show diagram" : "Show code";
  toggleBtn.setAttribute("aria-label", isCodeView ? "Show diagram" : "Show code");
}

function showMermaidFallback(diagramEl: HTMLElement, sourceEl: HTMLElement, toggleBtn: HTMLButtonElement): void {
  diagramEl.innerHTML = "";
  diagramEl.style.display = "none";
  sourceEl.style.display = "";
  toggleBtn.style.display = "none";
}

/**
 * Scans a container for mermaid code blocks and asynchronously renders valid diagrams.
 * Gracefully preserves the source code view if syntax is incomplete or invalid.
 */
export async function renderMermaidBlocks(container: HTMLElement): Promise<void> {
  const blocks = container.querySelectorAll<HTMLElement>("[data-mermaid-block]");
  if (blocks.length === 0) return;

  const tasks: Promise<void>[] = [];

  for (const block of blocks) {
    const codeEl = block.querySelector<HTMLElement>("pre > code");
    const code = codeEl?.textContent?.trim();
    if (!code) continue;

    const diagramEl = block.querySelector<HTMLElement>("[data-mermaid-diagram]");
    const sourceEl = block.querySelector<HTMLElement>("[data-mermaid-source]");
    const toggleBtn = block.querySelector<HTMLButtonElement>("[data-mermaid-toggle]");
    if (!diagramEl || !sourceEl || !toggleBtn) continue;

    if (block.dataset.renderedCode === code) {
      applyMermaidViewState(block, diagramEl, sourceEl, toggleBtn);
      continue;
    }

    const cached = svgCache.get(code);
    if (cached !== undefined) {
      if (!diagramEl.querySelector("svg")) {
        diagramEl.innerHTML = cached;
      }
      block.dataset.renderedCode = code;
      applyMermaidViewState(block, diagramEl, sourceEl, toggleBtn);
      continue;
    }

    const task = renderMermaidDiagram(code)
      .then((svg) => {
        const currentCode = block.querySelector<HTMLElement>("pre > code")?.textContent?.trim();
        if (currentCode !== code) return;

        if (svg !== null) {
          diagramEl.innerHTML = svg;
          block.dataset.renderedCode = code;
          applyMermaidViewState(block, diagramEl, sourceEl, toggleBtn);
        } else {
          showMermaidFallback(diagramEl, sourceEl, toggleBtn);
        }
      })
      .catch(() => {
        showMermaidFallback(diagramEl, sourceEl, toggleBtn);
      });

    tasks.push(task);
  }

  if (tasks.length > 0) {
    await Promise.all(tasks);
  }
}

/** Toggles between the rendered diagram and the underlying source code. */
export function toggleMermaidView(toggleBtn: HTMLButtonElement): void {
  const block = toggleBtn.closest<HTMLElement>("[data-mermaid-block]");
  if (!block) return;
  const diagramEl = block.querySelector<HTMLElement>("[data-mermaid-diagram]");
  const sourceEl = block.querySelector<HTMLElement>("[data-mermaid-source]");
  if (!diagramEl || !sourceEl) return;

  const isShowingDiagram = diagramEl.style.display !== "none";
  block.dataset.view = isShowingDiagram ? "code" : "diagram";
  applyMermaidViewState(block, diagramEl, sourceEl, toggleBtn);
}

/** Clears the diagram SVG cache (primarily for tests). */
export function clearMermaidCache(): void {
  svgCache.clear();
}
