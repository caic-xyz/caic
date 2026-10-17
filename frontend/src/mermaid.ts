// Lazy-loaded Pele renderer for Mermaid syntax: bounded SVG caching, safe insertion, and source toggling.

import DOMPurify from "dompurify";
import type * as peleApi from "pele/lazy";

type Pele = typeof peleApi;

let pelePromise: Promise<Pele> | null = null;
let nextDiagramId = 1;

const svgCache = new Map<string, string>();
const MAX_CACHE_SIZE = 100;
const MAX_SOURCE_LENGTH = 50_000;

function loadPele(): Promise<Pele> {
  pelePromise ??= import("pele/lazy").catch((error: unknown) => {
    pelePromise = null;
    throw error;
  });
  return pelePromise;
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
  // Bound input before Pele's lazy type detection preprocesses the source.
  if (!trimmed || trimmed.length > MAX_SOURCE_LENGTH) return null;

  const cached = svgCache.get(trimmed);
  if (cached !== undefined) return cached;

  let pele: Pele | null = null;
  try {
    pele = await loadPele();
    const { svg } = await pele.renderAsync(trimmed, {
      autoDirection: false,
      fontFamily: "system-ui",
      images: false,
      links: false,
      maxEdges: 500,
      outputLimit: 4_000_000,
      responsive: true,
    });
    // Diagram SVG bypasses Markdown's HTML-only sanitizer. Preserve SVG styling,
    // but disallow embedded documents, remote resources, navigation, and CSS sheets.
    const safeSvg = DOMPurify.sanitize(svg, {
      USE_PROFILES: { svg: true, svgFilters: true },
      FORBID_TAGS: ["a", "foreignObject", "image", "style"],
    });

    if (svgCache.size >= MAX_CACHE_SIZE) {
      const oldest = svgCache.keys().next().value;
      if (oldest !== undefined) svgCache.delete(oldest);
    }
    svgCache.set(trimmed, safeSvg);
    return safeSvg;
  } catch (error) {
    if (!pele || !(error instanceof pele.PeleError)) console.error("Failed to render Mermaid diagram", error);
    return null;
  }
}

function insertMermaidSvg(diagramEl: HTMLElement, svg: string): void {
  diagramEl.innerHTML = svg;
  const root = diagramEl.querySelector("svg");
  if (!root) return;
  // Cached drawings can appear several times in one document. Pele uses IDs for
  // accessible titles/descriptions; give each inserted instance its own names.
  const prefix = `mermaid_svg_${nextDiagramId++}`;
  for (const [tag, attribute] of [
    ["title", "aria-labelledby"],
    ["desc", "aria-describedby"],
  ] as const) {
    const label = root.querySelector(tag);
    if (label?.id) {
      label.id = `${prefix}_${tag}`;
      root.setAttribute(attribute, label.id);
    }
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
        insertMermaidSvg(diagramEl, cached);
      }
      block.dataset.renderedCode = code;
      applyMermaidViewState(block, diagramEl, sourceEl, toggleBtn);
      continue;
    }

    const task = renderMermaidDiagram(code).then((svg) => {
      const currentCode = block.querySelector<HTMLElement>("pre > code")?.textContent?.trim();
      if (!container.contains(block) || currentCode !== code) return;

      if (svg !== null) {
        insertMermaidSvg(diagramEl, svg);
        block.dataset.renderedCode = code;
        applyMermaidViewState(block, diagramEl, sourceEl, toggleBtn);
      } else {
        showMermaidFallback(diagramEl, sourceEl, toggleBtn);
      }
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
