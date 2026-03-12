import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  webServer: {
    command: "go run -tags e2e ../backend/cmd/caic",
    url: "http://localhost:8090/api/v1/server/repos",
    reuseExistingServer: false,
    timeout: 30_000,
  },
  use: {
    baseURL: "http://localhost:8090",
  },
  projects: [{ name: "chromium", use: { channel: "chrome" } }],
});
