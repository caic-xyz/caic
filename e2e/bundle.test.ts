// Tests frontend bundle reuse, fingerprint invalidation, and retry after a failed build.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { staleBundle } from "../scripts/frontend_bundle";

const lockfile = "pnpm-lock.yaml";
const asset = "frontend/public/sw.js";
const sdk = "sdk/caic/ts/v1/api.gen.ts";
const sdkConfig = "sdk/caic/ts/tsconfig.json";
const compressedEntry = "backend/frontend/dist/index.html.br";

function tree(t: TestContext) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "caic-bundle-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  function write(file: string, contents: string): void {
    const target = path.join(root, file);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, contents);
  }
  for (const file of [
    "frontend/index.html",
    asset,
    "frontend/src/app.ts",
    "frontend/src/app.test.ts",
    "node_modules/@gomodedev/gomode/package.json",
    "node_modules/@gomodedev/gomode/sdk/gomode/ts/api.gen.ts",
    "node_modules/@gomodedev/gomode/sdk/mcp/ts/api.gen.ts",
    "node_modules/@gomodedev/gomode/sdk/oauth/ts/api.gen.ts",
    "node_modules/@gomodedev/gomode/sdk/voicegateway/ts/api.gen.ts",
    "node_modules/@gomodedev/gomode/web/assets/sample.json",
    "node_modules/@gomodedev/gomode/web/src/app.ts",
    lockfile,
    "pnpm-workspace.yaml",
    "pyproject.toml",
    "scripts/css_module_types.mjs",
    "scripts/precompress_dist.py",
    sdk,
    sdkConfig,
    "tsconfig.json",
    "uv.lock",
    "vite.config.ts",
  ]) {
    write(file, "");
  }
  write("package.json", '{"type":"module"}');
  fs.copyFileSync(
    path.resolve(import.meta.dirname, "../scripts/frontend_bundle.ts"),
    path.join(root, "scripts/frontend_bundle.ts"),
  );
  write(
    "bin/vite.mjs",
    `import fs from "node:fs";
const dist = "backend/frontend/dist";
fs.rmSync(dist, { recursive: true, force: true });
fs.mkdirSync(dist + "/.vite", { recursive: true });
fs.writeFileSync(${JSON.stringify(compressedEntry)}, "bundle");
fs.appendFileSync("builds", ".");`,
  );
  write("bin/uv.mjs", 'if (process.env.CAIC_TEST_COMPRESS_FAIL === "1") process.exitCode = 1;');
  for (const tool of ["vite", "uv"]) {
    if (process.platform === "win32") write(`bin/${tool}.cmd`, `@"%CAIC_TEST_NODE%" "bin/${tool}.mjs"\r\n`);
    else {
      write(`bin/${tool}`, `#!/bin/sh\nexec "$CAIC_TEST_NODE" bin/${tool}.mjs\n`);
      fs.chmodSync(path.join(root, "bin", tool), 0o755);
    }
  }
  function build(failCompression: boolean): void {
    const result = spawnSync(process.execPath, ["scripts/frontend_bundle.ts"], {
      cwd: root,
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${path.join(root, "bin")}${path.delimiter}${process.env.PATH ?? ""}`,
        CAIC_TEST_NODE: process.execPath,
        CAIC_TEST_COMPRESS_FAIL: failCompression ? "1" : "0",
      },
    });
    assert.equal(result.status, failCompression ? 1 : 0, result.stderr);
  }
  return { root, write, build };
}

test("reuses an unchanged build even if timestamps or tests change", (t) => {
  const { root, write, build } = tree(t);
  write(compressedEntry, "prebuilt checkout");
  assert.notEqual(staleBundle(root), null);
  build(false);
  write(sdk, "");
  write("frontend/src/app.test.ts", "test-only change");
  build(false);
  assert.equal(staleBundle(root), null);
  assert.equal(fs.readFileSync(path.join(root, "builds"), "utf8"), ".");
});

test("rebuilds when SDKs, the lockfile, or the asset inventory change", (t) => {
  const { root, write, build } = tree(t);
  build(false);
  write(sdk, "new SDK");
  assert.equal(staleBundle(root), "frontend build inputs changed");
  build(false);
  write(sdkConfig, '{"compilerOptions":{"target":"ESNext"}}');
  assert.equal(staleBundle(root), "frontend build inputs changed");
  build(false);
  write(lockfile, "new lockfile");
  assert.equal(staleBundle(root), "frontend build inputs changed");
  build(false);
  fs.unlinkSync(path.join(root, asset));
  fs.rmdirSync(path.join(root, "frontend/public"));
  assert.equal(staleBundle(root), "frontend build inputs changed");
  build(false);
  fs.unlinkSync(path.join(root, compressedEntry));
  assert.equal(staleBundle(root), "frontend build output changed");
  build(false);
  assert.equal(staleBundle(root), null);
  assert.equal(fs.readFileSync(path.join(root, "builds"), "utf8"), "......");
});

test("retries compression failures without accepting the partial output", (t) => {
  const { root, write, build } = tree(t);
  build(false);
  write(sdk, "new SDK");
  build(true);
  assert.notEqual(staleBundle(root), null);
  build(false);
  assert.equal(staleBundle(root), null);
  assert.equal(fs.readFileSync(path.join(root, "builds"), "utf8"), "...");
});
