import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  webServer: {
    command: "../scripts/run-dev.py --http :8090 --fake",
    url: "http://localhost:8090/api/v1/server/config",
    reuseExistingServer: false,
    timeout: 30_000,
  },
  use: {
    baseURL: "http://localhost:8090",
  },
  projects: [{ name: "chromium", use: { channel: "chrome" } }],
});
