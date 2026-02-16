import { resolve } from "path";
import { defineConfig } from "vitest/config";
import solidPlugin from "vite-plugin-solid";

export default defineConfig({
  plugins: [solidPlugin()],
  resolve: {
    alias: {
      "@sdk": resolve(__dirname, "sdk"),
    },
  },
  test: {
    root: "frontend",
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test-setup.ts"],
  },
});
