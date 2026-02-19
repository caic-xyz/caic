import { resolve } from "path";
import { defineConfig } from "vitest/config";
import solidPlugin from "vite-plugin-solid";
import solidSVG from "vite-solid-svg";

export default defineConfig({
  plugins: [solidPlugin(), solidSVG()],
  resolve: {
    alias: {
      "@sdk": resolve(__dirname, "sdk/ts"),
    },
  },
  test: {
    root: "frontend",
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test-setup.ts"],
  },
});
