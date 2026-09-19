// Vite configuration for the SolidJS frontend: plugins, path aliases, and the
// production build policy shared with the other caic frontends.

import { resolve } from "path";
import { defineConfig } from "vite";
import solidPlugin from "vite-plugin-solid";
import solidSVG from "vite-solid-svg";

export default defineConfig({
  root: "frontend",
  logLevel: "warn",
  plugins: [solidPlugin(), solidSVG()],
  resolve: {
    // A duplicated solid-js copy breaks reactivity signal identity at runtime.
    dedupe: ["solid-js"],
    alias: {
      "@mcp-sdk": resolve(import.meta.dirname, "sdk/mcp/ts/v1"),
      "@sdk": resolve(import.meta.dirname, "sdk/caic/ts/v1"),
      "@voicegateway-sdk": resolve(import.meta.dirname, "sdk/voicegateway/ts/v1"),
    },
  },
  build: {
    // Matches the browsers Vite 8 supports by default; pinned so a Vite upgrade
    // cannot silently move the floor.
    target: "baseline-widely-available",
    minify: "oxc",
    cssMinify: "lightningcss",
    outDir: "../backend/frontend/dist",
    emptyOutDir: true,
    reportCompressedSize: false,
    // The bundle is brotli-compressed into the Go binary, so a source map would
    // be embedded as dead weight. Debug against the dev server instead.
    sourcemap: false,
  },
  server: {
    proxy: {
      "/api": "http://localhost:2242",
    },
  },
});
