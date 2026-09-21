// End-to-end browser test configuration using a fake backend server.
//
// The fake server (scripts/run-dev.py --fake) replaces containers, runtime
// inventory, runtime events, CI, usage providers, VNC, and agent processes
// with deterministic fakes, so the suite never depends on Docker, Podman, md,
// SSH, external LLMs, or network credentials. A smoke test for the real
// runtime must not use it; see backend/cmd/caic/smoke_test.go.
//
// Playwright transpiles the TypeScript itself, so e2e/tsconfig.json exists
// solely for `pnpm typecheck`, which checks both the root project
// (frontend/src, sdk/) and this directory. Note that Playwright matchers like
// `toContain` accept `unknown`, so assertion arguments are not type checked.
//
// Never run this suite concurrently with itself or with frontend builds
// (`make build`, screenshots targets): they share backend/frontend/dist, so
// simultaneous builds can delete or replace one another's generated assets.
// Always run through this config (`make test-e2e`); a bare
// `pnpm playwright test` bypasses it. For a targeted run keep the explicit
// configuration:
//   pnpm exec playwright test --config e2e/playwright.config.ts e2e/tests/account-menu.spec.ts
//
// The webServer dynamically allocates a port for the fake backend and verifies
// it before running instrumented tests.

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
  // The dot reporter keeps progress to one line; failures still print in full at the end.
  reporter: "dot",
  webServer: {
    // Both streams go to the log file instead of the terminal: the fake server is
    // chatty and the dot reporter keeps the run to one line. The test-e2e make
    // target prints the log tail when the suite fails.
    command: `mkdir -p ../test-results && ../scripts/run-dev.py --http ${serverHost}:${serverPort} --fake > ../test-results/e2e-server.log 2>&1`,
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
