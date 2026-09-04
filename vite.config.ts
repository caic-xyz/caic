// Bundler configuration for the SolidJS frontend, including plugins and path aliases.

import { resolve } from "path";
import { defineConfig } from "vite";
import solidPlugin from "vite-plugin-solid";
import solidSVG from "vite-solid-svg";

export default defineConfig({
  root: "frontend",
  logLevel: "warn",
  plugins: [solidPlugin(), solidSVG()],
  resolve: {
    alias: {
      "@mcp-sdk": resolve(import.meta.dirname, "sdk/mcp/ts/v1"),
      "@sdk": resolve(import.meta.dirname, "sdk/caic/ts/v1"),
      "@voicegateway-sdk": resolve(import.meta.dirname, "sdk/voicegateway/ts/v1"),
    },
  },
  build: {
    outDir: "../backend/frontend/dist",
    emptyOutDir: true,
    reportCompressedSize: false,
  },
  server: {
    proxy: {
      "/api": "http://localhost:2242",
    },
  },
});
