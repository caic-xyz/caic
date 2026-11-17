// Tests initial bundle accounting, lazy exclusions, missing-build failures, and CLI budget enforcement.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { brotliCompressSync, constants } from "node:zlib";
import { measureStartup } from "./startup_budget.mjs";

const fixtureCompression = { params: { [constants.BROTLI_PARAM_QUALITY]: 1 } };

const script = resolve("scripts/startup_budget.mjs");
function run(args) {
  return spawnSync(process.execPath, [script, ...args], { encoding: "utf8" });
}
async function fixture(t, manifest, files) {
  const dist = await mkdtemp(join(tmpdir(), "caic-startup-budget-"));
  t.after(() => rm(dist, { recursive: true, force: true }));
  const sizes = {};
  for (const [file, body] of Object.entries({ ".vite/manifest.json": JSON.stringify(manifest), ...files })) {
    const path = join(dist, `${file}.br`);
    await mkdir(dirname(path), { recursive: true });
    const compressed = brotliCompressSync(Buffer.from(body), fixtureCompression);
    await writeFile(path, compressed);
    sizes[file] = { brotli: compressed.length, decoded: Buffer.byteLength(body) };
  }
  return { dist, sizes };
}

test("counts transitive static imports and shared CSS once, excluding dynamic-only files", async (t) => {
  const { dist, sizes } = await fixture(
    t,
    {
      "index.html": {
        isEntry: true,
        file: "assets/main.js",
        imports: ["shared", "view"],
        css: ["assets/base.css"],
        dynamicImports: ["lazy"],
      },
      shared: { file: "assets/shared.js", css: ["assets/shared.css"] },
      view: { file: "assets/view.mjs", imports: ["shared", "index.html"], css: ["assets/base.css"] },
      lazy: { file: "assets/not-present-lazy.js", css: ["assets/not-present-lazy.css"] },
    },
    {
      "assets/main.js": 'import "./shared.js"; export { value } from "./view.mjs"; import("./not-present-lazy.js");',
      "assets/shared.js": "export const shared=1;",
      "assets/view.mjs": 'export * from "./shared.js";',
      "assets/base.css": ".base{display:block}",
      "assets/shared.css": ".shared{color:inherit}",
    },
  );
  const [scripts, styles] = measureStartup(dist);
  assert.deepEqual(
    scripts.assets.map((asset) => asset.file),
    ["assets/main.js", "assets/shared.js", "assets/view.mjs"],
  );
  assert.equal(
    scripts.brotli,
    sizes["assets/main.js"].brotli + sizes["assets/shared.js"].brotli + sizes["assets/view.mjs"].brotli,
  );
  assert.equal(
    scripts.decoded,
    sizes["assets/main.js"].decoded + sizes["assets/shared.js"].decoded + sizes["assets/view.mjs"].decoded,
  );
  assert.equal(styles.brotli, sizes["assets/base.css"].brotli + sizes["assets/shared.css"].brotli);
  assert.equal(styles.decoded, sizes["assets/base.css"].decoded + sizes["assets/shared.css"].decoded);
  const checked = run(["--dist", dist]);
  assert.equal(checked.status, 0);
  assert.equal(checked.stdout, "");
  assert.equal(checked.stderr, "");
  const reported = run(["--dist", dist, "--report"]);
  assert.equal(reported.status, 0);
  assert.match(reported.stdout, /JavaScript brotli:/);
  assert.match(reported.stdout, /CSS decoded:/);
  assert.equal(reported.stderr, "");
  assert.equal((await readFile(join(dist, "assets/main.js.br"))).length, sizes["assets/main.js"].brotli);
});

test("fails closed for absent entry, missing static metadata/body, and invalid dependency lists", async (t) => {
  for (const [manifest, files, message] of [
    [{}, {}, /Missing index.html/],
    [
      { "index.html": { isEntry: true, file: "assets/main.js", imports: ["missing"] } },
      { "assets/main.js": "" },
      /static manifest chunk: missing/,
    ],
    [{ "index.html": { isEntry: true, file: "assets/missing.js" } }, {}, /ENOENT/],
    [{ "index.html": { isEntry: true, file: "assets/main.css" } }, {}, /Unsupported initial chunk type/],
    [{ "index.html": { isEntry: true, file: "assets/main.cjs" } }, {}, /Unsupported initial chunk type/],
    [
      {
        "index.html": { isEntry: true, file: "assets/main.js", imports: ["unsupported"] },
        unsupported: { file: "assets/shared.cjs" },
      },
      { "assets/main.js": "" },
      /Unsupported initial chunk type/,
    ],
    [
      { "index.html": { isEntry: true, file: "assets/main.js", imports: "shared" } },
      { "assets/main.js": "" },
      /static imports/,
    ],
    [
      { "index.html": { isEntry: true, file: "assets/main.js", css: [null] } },
      { "assets/main.js": "" },
      /CSS dependencies/,
    ],
    [{ "index.html": { isEntry: true, file: "../escape.js" } }, {}, /escapes/],
  ]) {
    const { dist } = await fixture(t, manifest, files);
    const result = run(["--dist", dist]);
    assert.equal(result.status, 1);
    assert.match(result.stderr, message);
  }
});

test("rejects corrupt compressed input and malformed manifest JSON", async (t) => {
  const { dist } = await fixture(t, {}, {});
  await writeFile(join(dist, ".vite/manifest.json.br"), "not brotli");
  assert.equal(run(["--dist", dist]).status, 1);
  await writeFile(join(dist, ".vite/manifest.json.br"), brotliCompressSync(Buffer.from("{broken"), fixtureCompression));
  const result = run(["--dist", dist]);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Startup measurement failed/);
});

test("decoded JavaScript and CSS regressions fail the build with actionable overage", async (t) => {
  for (const [file, body, name] of [
    ["assets/main.js", `export const data='${"x".repeat(700_000)}';`, "JavaScript"],
    ["assets/main.css", `.large{--data:'${"x".repeat(150_000)}'}`, "CSS"],
  ]) {
    const isCSS = name === "CSS";
    const { dist } = await fixture(
      t,
      {
        "index.html": { isEntry: true, file: "assets/entry.js", ...(isCSS ? { css: [file] } : { imports: ["large"] }) },
        large: { file },
      },
      { "assets/entry.js": "", [file]: body },
    );
    const result = run(["--dist", dist]);
    assert.equal(result.status, 1);
    assert.equal(result.stdout, "");
    assert.match(result.stderr, new RegExp(`${name} decoded:.*over budget`));
    assert.match(result.stderr, /Defer optional code/);
    assert.ok(result.stderr.includes(file));
  }
});

test("Brotli regressions fail even when decoded bodies remain within budget", async (t) => {
  for (const [name, bytes] of [
    ["JavaScript", 250_000],
    ["CSS", 40_000],
  ]) {
    const payload = createHash("shake256", { outputLength: bytes }).update("startup-budget-fixture").digest("base64");
    const isCSS = name === "CSS";
    const file = `assets/large.${isCSS ? "css" : "js"}`;
    const body = isCSS ? `.large{--data:'${payload}'}` : `export const data='${payload}';`;
    const { dist } = await fixture(
      t,
      {
        "index.html": { isEntry: true, file: "assets/entry.js", ...(isCSS ? { css: [file] } : { imports: ["large"] }) },
        large: { file },
      },
      { "assets/entry.js": "", [file]: body },
    );
    const result = run(["--dist", dist]);
    assert.equal(result.status, 1);
    assert.equal(result.stdout, "");
    assert.match(result.stderr, new RegExp(`${name} brotli:.*over budget`));
    assert.match(result.stderr, new RegExp(`${name} decoded:.*remaining`));
  }
});

test("CLI supports help and rejects unknown or incomplete arguments", () => {
  for (const flag of ["--help", "-h"]) {
    const result = run([flag]);
    assert.equal(result.status, 0);
    assert.match(result.stdout, /Usage:/);
  }
  for (const [args, error] of [
    [[], "Missing required --dist <built-directory>."],
    [["--dist"], "Missing required --dist <built-directory>."],
    [["--dist", ""], "Missing required --dist <built-directory>."],
    [["--dist", "--help"], "Missing required --dist <built-directory>."],
    [["--unknown", "."], "Unknown argument: --unknown."],
    [["--dist", ".", "extra"], "Unexpected argument: extra."],
  ]) {
    const result = run(args);
    assert.equal(result.status, 2);
    assert.equal(result.stdout, "");
    assert.ok(result.stderr.startsWith(`${error}\nUsage:`));
    assert.match(result.stderr, /Usage:/);
  }
});
