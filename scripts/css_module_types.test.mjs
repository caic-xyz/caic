// Tests CSS export typing, stale-output checks, deleted modules, and live declaration refresh.
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm, unlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { once } from "node:events";
import { setTimeout } from "node:timers/promises";
import { test } from "node:test";
import ts from "typescript";

const script = resolve("scripts/css_module_types.mjs");
function run(source, mode) {
  return spawnSync(process.execPath, [script, "--source-path", source, ...mode], { encoding: "utf8" });
}
async function fixture(t) {
  const source = await mkdtemp(join(tmpdir(), "css-module-types-"));
  t.after(() => rm(source, { recursive: true, force: true }));
  return source;
}
async function waitFor(check) {
  const deadline = Date.now() + 10000;
  while (!(await check())) {
    if (Date.now() >= deadline) throw new Error("CSS types watcher did not update");
    await setTimeout(25);
  }
}
function declaredKeys(content) {
  const source = ts.createSourceFile("style.d.ts", content, ts.ScriptTarget.Latest);
  const statement = source.statements.find(ts.isVariableStatement);
  const type = statement?.declarationList.declarations[0]?.type;
  assert.ok(type && ts.isTypeLiteralNode(type));
  return type.members.map((member) => member.name?.text).sort();
}

const css = `@value gap: 4px;
.local { composes: base from './base.module.css'; }
:global(.external) .child { color: red; }
:global(.standalone) { color: blue; }
.dash-name { color: green; }
:export { duration: 5; }
@keyframes pulse { from { opacity: 0; } to { opacity: 1; } }
`;
async function compositionFixture(t) {
  const source = await fixture(t);
  await writeFile(join(source, "base.module.css"), ".base { color: red; }\n");
  await writeFile(join(source, "style.module.css"), css);
  assert.equal(run(source, ["--write"]).status, 0);
  return source;
}

test("CSS exports reject unknown classes even with a bundler wildcard declaration", async (t) => {
  const source = await compositionFixture(t);
  assert.deepEqual(declaredKeys(await readFile(join(source, "style.module.css.d.ts"), "utf8")), [
    "child",
    "dash-name",
    "duration",
    "gap",
    "local",
    "pulse",
  ]);
  await writeFile(
    join(source, "wildcard.d.ts"),
    'declare module "*.module.css" { const styles: Record<string,string>; export default styles; }',
  );
  const entry = join(source, "consumer.ts");
  await writeFile(
    entry,
    'import styles from "./style.module.css"; const valid: string = styles.local; void valid; styles.misspelled;',
  );
  const program = ts.createProgram([entry, join(source, "wildcard.d.ts")], {
    noEmit: true,
    strict: true,
    noUncheckedIndexedAccess: true,
    module: ts.ModuleKind.ESNext,
    moduleResolution: ts.ModuleResolutionKind.Bundler,
    types: [],
    skipLibCheck: true,
  });
  const diagnostics = ts.getPreEmitDiagnostics(program);
  assert.equal(
    diagnostics.length,
    1,
    diagnostics.map((item) => ts.flattenDiagnosticMessageText(item.messageText, "\n")).join("\n"),
  );
  assert.equal(diagnostics[0]?.code, 2339);
  assert.match(ts.flattenDiagnosticMessageText(diagnostics[0].messageText, "\n"), /misspelled/);
});

test("check mode rejects missing, changed, and orphaned types without writing", async (t) => {
  const source = await fixture(t);
  const cssPath = join(source, "style.module.css");
  const declaration = `${cssPath}.d.ts`;
  await writeFile(cssPath, ".original {}\n");
  assert.equal(run(source, []).status, 1);
  assert.equal(run(source, ["--write"]).status, 0);
  const before = await readFile(declaration, "utf8");
  assert.equal(run(source, []).status, 0);
  await writeFile(cssPath, ".renamed {}\n");
  assert.equal(run(source, []).status, 1);
  assert.equal(await readFile(declaration, "utf8"), before);
  assert.equal(run(source, ["--write"]).status, 0);
  await unlink(cssPath);
  const orphan = await readFile(declaration, "utf8");
  assert.equal(run(source, []).status, 1);
  assert.equal(await readFile(declaration, "utf8"), orphan);
  assert.equal(run(source, ["--write"]).status, 0);
  await assert.rejects(readFile(declaration), { code: "ENOENT" });
});

test("watch mode follows created, edited, and deleted modules", async (t) => {
  const source = await fixture(t);
  const child = spawn(process.execPath, [script, "--source-path", source, "--watch"], {
    stdio: ["ignore", "pipe", "pipe"],
  });
  const stopped = once(child, "close");
  t.after(async () => {
    child.kill();
    await stopped;
  });
  let output = "";
  let errors = "";
  child.stdout.on("data", (data) => {
    output += data;
  });
  child.stderr.on("data", (data) => {
    errors += data;
  });
  await waitFor(() => output.includes("Watching CSS Module types"));
  const cssPath = join(source, "new.module.css");
  const declaration = `${cssPath}.d.ts`;
  async function content() {
    try {
      return await readFile(declaration, "utf8");
    } catch (error) {
      if (error.code === "ENOENT") return null;
      throw error;
    }
  }
  await writeFile(cssPath, ".first {}\n");
  await waitFor(async () => (await content())?.includes('"first"'));
  await writeFile(cssPath, ".second {}\n");
  await waitFor(async () => (await content())?.includes('"second"'));
  await unlink(cssPath);
  await waitFor(async () => (await content()) === null);
  assert.equal(errors, "");
});

test("declarations match Vite exports for compositions, globals, and ICSS values", async (t) => {
  const source = await compositionFixture(t);
  const entry = join(source, "entry.js");
  await writeFile(entry, 'import styles from "./style.module.css"; export default styles;');
  const { build } = await import("vite");
  const result = await build({
    configFile: false,
    root: source,
    logLevel: "silent",
    build: {
      write: false,
      minify: false,
      lib: { entry, formats: ["es"], fileName: "fixture", cssFileName: "fixture" },
    },
  });
  const outputs = Array.isArray(result) ? result.flatMap((item) => item.output) : result.output;
  const chunk = outputs.find((item) => item.type === "chunk" && item.isEntry);
  assert.ok(chunk);
  const actual = await import(`data:text/javascript;base64,${Buffer.from(chunk.code).toString("base64")}`);
  const declaration = await readFile(join(source, "style.module.css.d.ts"), "utf8");
  assert.deepEqual(declaredKeys(declaration), Object.keys(actual.default).sort());
  assert.equal(actual.default.duration, "5");
  assert.equal(actual.default.local.split(" ").length, 2);
});
