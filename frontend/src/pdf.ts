// Loads PDF.js with bundled worker, font, CMap, and image-decoder assets on demand.
import { GlobalWorkerOptions, getDocument } from "pdfjs-dist";
import workerURL from "pdfjs-dist/build/pdf.worker.mjs?url";

GlobalWorkerOptions.workerSrc = workerURL;

// Vite hashes each asset; PDF.js requests their original names through its
// public BinaryDataFactory hook. Keep the lookup inside the lazy PDF chunk.
const assetURLs = import.meta.glob<string>(
  "../../node_modules/pdfjs-dist/{cmaps/*.bcmap,standard_fonts/*.{pfb,ttf},wasm/{jbig2,openjpeg}.wasm}",
  { eager: true, query: "?url", import: "default" },
);
const directories: Record<string, string> = {
  cMapUrl: "cmaps",
  standardFontDataUrl: "standard_fonts",
  wasmUrl: "wasm",
};

export function loadPdf(url: string, signal: AbortSignal) {
  class BinaryDataFactory {
    async fetch({ kind, filename }: { kind: string; filename: string }): Promise<Uint8Array> {
      const assetURL = assetURLs[`../../node_modules/pdfjs-dist/${directories[kind]}/${filename}`];
      if (!assetURL) throw new Error(`Missing PDF asset: ${filename}`);
      const response = await fetch(assetURL, { signal });
      if (!response.ok) throw new Error(`Failed to load PDF asset: ${filename} (HTTP ${response.status})`);
      return new Uint8Array(await response.arrayBuffer());
    }
  }
  return getDocument({ url, BinaryDataFactory, useWorkerFetch: false });
}
