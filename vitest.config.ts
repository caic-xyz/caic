// Unit test runner configuration for the SolidJS frontend using Vitest.

import { resolve } from "path";
import { defineConfig } from "vitest/config";
import solidPlugin from "vite-plugin-solid";
import solidSVG from "vite-solid-svg";

export default defineConfig({
  plugins: [solidPlugin(), solidSVG()],
  resolve: {
    alias: {
      "@sdk": resolve(__dirname, "sdk/caic/ts/v1"),
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
        "frontend/src/test-setup.ts",
        "frontend/src/**/*.test.ts",
        "frontend/src/**/*.test.tsx",
        "frontend/src/css.d.ts",
        "frontend/src/novnc.d.ts",
        "frontend/src/**/*.svg",
        "frontend/src/**/*.module.css",
      ],
      thresholds: {
        // Informational only — not enforced.
        lines: 0,
        branches: 0,
      },
    },
  },
});
