// Tests that a frontend bundle older than any bundled input is reported stale.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { bundleStamp, staleBundle } from "./bundle";

const inputs = [
  "frontend/index.html",
  "frontend/public/sw.js",
  "frontend/src/app.ts",
  "package.json",
  "sdk/caic/ts/v1/types.gen.ts",
  "vite.config.ts",
];

function tree(t: TestContext): string {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "caic-bundle-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const file of [...inputs, "frontend/src/app.test.ts", bundleStamp]) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true });
    fs.writeFileSync(path.join(root, file), "");
    fs.utimesSync(path.join(root, file), 1_000, 1_000);
  }
  fs.utimesSync(path.join(root, bundleStamp), 2_000, 2_000);
  return root;
}

test("staleBundle", async (t) => {
  await t.test("accepts a bundle built after every input", (t) => {
    assert.equal(staleBundle(tree(t)), null);
  });

  await t.test("reports an input edited after the build", (t) => {
    const root = tree(t);
    fs.utimesSync(path.join(root, "frontend/src/app.ts"), 3_000, 3_000);
    assert.equal(staleBundle(root), "frontend/src/app.ts changed after the last frontend build");
  });

  await t.test("ignores edited tests, which are not bundled", (t) => {
    const root = tree(t);
    fs.utimesSync(path.join(root, "frontend/src/app.test.ts"), 3_000, 3_000);
    assert.equal(staleBundle(root), null);
  });

  await t.test("reports a missing bundle", (t) => {
    const root = tree(t);
    fs.rmSync(path.join(root, bundleStamp));
    assert.equal(staleBundle(root), `${bundleStamp} is missing`);
  });
});
