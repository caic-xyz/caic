// Unit test runner configuration for the SolidJS frontend using Vitest.

import { resolve } from "path";
import { defineConfig } from "vitest/config";
import solidPlugin from "vite-plugin-solid";
import solidSVG from "vite-solid-svg";

export default defineConfig({
  plugins: [solidPlugin(), solidSVG()],
  resolve: {
    alias: {
      "@mcp-sdk": resolve(__dirname, "sdk/mcp/ts/v1"),
      "@sdk": resolve(__dirname, "sdk/caic/ts/v1"),
      "@voicegateway-sdk": resolve(__dirname, "sdk/voicegateway/ts/v1"),
    },
  },
  test: {
    root: "frontend",
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test-setup.ts"],
    coverage: {
      provider: "v8",
      reporter: ["text", "html", "lcov"],
      include: ["frontend/src/**"],
      exclude: [
        "frontend/src/**/*.module.css",
        "frontend/src/**/*.svg",
        "frontend/src/**/*.test.ts",
        "frontend/src/**/*.test.tsx",
        "frontend/src/css.d.ts",
        "frontend/src/novnc.d.ts",
        "frontend/src/test-setup.ts",
      ],
      thresholds: {
        // Informational only — not enforced.
        lines: 0,
        branches: 0,
      },
    },
  },
});
