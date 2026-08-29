// End-to-end browser test configuration using a fake backend server.

import { defineConfig } from "@playwright/test";

const seed = process.env.CAIC_E2E_SEED ?? Date.now().toString(36);
process.env.CAIC_E2E_SEED = seed;
if (process.env.TEST_WORKER_INDEX === undefined) {
  console.log(`E2E seed: ${seed} (replay with CAIC_E2E_SEED=${seed})`);
}

export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  webServer: {
    command: "../scripts/run-dev.py --http :8090 --fake",
    url: "http://localhost:8090/api/caic/v1/server/config",
    reuseExistingServer: false,
    timeout: 30_000,
  },
  use: {
    baseURL: "http://localhost:8090",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { channel: "chrome" } }],
});
