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
      "@sdk": resolve(__dirname, "sdk/caic/ts/v1"),
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
