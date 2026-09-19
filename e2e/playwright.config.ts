// End-to-end browser test configuration using a fake backend server.

import { defineConfig } from "@playwright/test";

const seed = process.env.CAIC_E2E_SEED ?? Date.now().toString(36);
const includeVisuals = process.env.CAIC_E2E_VISUALS === "1";
const serverHost = process.env.CAIC_E2E_HOST ?? "127.0.0.1";
if (serverHost !== "127.0.0.1") {
  throw new Error(`CAIC_E2E_HOST must be 127.0.0.1, got ${serverHost}`);
}
const serverPort = process.env.CAIC_E2E_PORT ?? "8090";
if (!/^\d+$/.test(serverPort)) {
  throw new Error(`CAIC_E2E_PORT must be a decimal port, got ${serverPort}`);
}
const serverURL = `http://${serverHost}:${serverPort}`;
process.env.CAIC_E2E_SEED = seed;
if (process.env.TEST_WORKER_INDEX === undefined) {
  console.log(`E2E seed: ${seed} (replay with CAIC_E2E_SEED=${seed})`);
}

export default defineConfig({
  testDir: "./tests",
  testIgnore: includeVisuals ? [] : ["**/gen-screenshots.spec.ts", "**/prompt-input.spec.ts"],
  timeout: 60_000,
  outputDir: process.env.CAIC_E2E_OUTPUT_DIR,
  webServer: {
    command: `../scripts/run-dev.py --http ${serverHost}:${serverPort} --fake`,
    url: `${serverURL}/api/caic/v1/server/config`,
    reuseExistingServer: false,
    timeout: 30_000,
  },
  use: {
    baseURL: serverURL,
    colorScheme: "light",
    deviceScaleFactor: 1,
    launchOptions: includeVisuals
      ? {
          args: ["--disable-gpu", "--disable-gpu-rasterization", "--num-raster-threads=1"],
        }
      : undefined,
    locale: "en-US",
    timezoneId: "UTC",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium" }],
});
