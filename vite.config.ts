// Vite configuration for the SolidJS frontend: plugins, path aliases, and the
// production build policy shared with the other caic frontends.

import { resolve } from "path";
import { defineConfig, type Plugin } from "vite";
import solidPlugin from "vite-plugin-solid";
import solidSVG from "vite-solid-svg";

// occt-import-js ships Emscripten's default loader, which requires these Node
// builtins only inside ENVIRONMENT_IS_NODE branches that the STEP worker never
// runs. Resolve them to an empty module rather than Vite's browser-external stub
// and its build warning; other importers keep the warning. Frontend tests run
// the package under Node outside Vite and keep the real modules.
function occtNodeBuiltins(): Plugin {
  const stub = "\0occt-node-builtin";
  return {
    name: "caic:occt-node-builtins",
    enforce: "pre",
    resolveId: {
      filter: { id: /^(?:crypto|fs|path)$/ },
      handler(id, importer) {
        return importer?.includes("/node_modules/occt-import-js/") ? stub : null;
      },
    },
    load: {
      filter: { id: /^\0occt-node-builtin$/ },
      handler() {
        return "module.exports = {};";
      },
    },
  };
}

export default defineConfig({
  root: "frontend",
  logLevel: "warn",
  plugins: [solidPlugin(), solidSVG()],
  resolve: {
    // Linked hosts share signals and router contexts with the application.
    dedupe: ["@solidjs/router", "solid-js"],
    alias: {
      "@sdk": resolve(import.meta.dirname, "sdk/caic/ts/v1"),
    },
  },
  build: {
    // No <link rel="modulepreload"> hints. The caic service worker serves every
    // /assets/ request cache-first via respondWith, and Chrome refuses to reuse
    // an SW-served preload across script worlds ("cross-world service worker
    // resource mismatch"), so each hint would fetch its chunk twice and spam
    // "preloaded but not used" console warnings. The SW cache already makes
    // these chunks instant on repeat visits.
    modulePreload: false,
    // The startup budget walks Vite's static imports and CSS dependencies.
    // Keep this build-only metadata in .vite, which go:embed and git exclude.
    manifest: true,
    // Matches the browsers Vite 8 supports by default; pinned so a Vite upgrade
    // cannot silently move the floor.
    target: "baseline-widely-available",
    minify: "oxc",
    cssMinify: "lightningcss",
    outDir: "../backend/frontend/dist",
    emptyOutDir: true,
    reportCompressedSize: false,
    // The largest chunks, CAD (three.js) and PdfPreview (pdfjs-dist), load only
    // on demand; scripts/startup_budget.mjs gates the initial payload. This
    // limit only catches a heavy dependency landing in one chunk by accident.
    chunkSizeWarningLimit: 800,
    // The bundle is brotli-compressed into the Go binary, so a source map would
    // be embedded as dead weight. Debug against the dev server instead.
    sourcemap: false,
  },
  worker: {
    plugins: () => [occtNodeBuiltins()],
  },
  server: {
    proxy: {
      "/api": "http://localhost:2242",
    },
  },
});
