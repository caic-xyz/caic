// Builds the compressed frontend and checks its untracked fingerprint before reusing it.

import { execSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";

const dist = "backend/frontend/dist";
const marker = ".vite/bundle.sha256";
const inputs = [
  "frontend/index.html",
  "frontend/public",
  "frontend/src",
  "frontend/tsconfig.json",
  "node_modules/@gomodedev/gomode/package.json",
  "node_modules/@gomodedev/gomode/sdk/gomode/ts",
  "node_modules/@gomodedev/gomode/sdk/mcp/ts",
  "node_modules/@gomodedev/gomode/sdk/oauth/ts",
  "node_modules/@gomodedev/gomode/sdk/voicegateway/ts",
  "node_modules/@gomodedev/gomode/tsconfig.json",
  "node_modules/@gomodedev/gomode/web/assets",
  "node_modules/@gomodedev/gomode/web/src",
  "node_modules/@gomodedev/gomode/web/tsconfig.json",
  "package.json",
  "pnpm-lock.yaml",
  "pnpm-workspace.yaml",
  "pyproject.toml",
  "scripts/css_module_types.mjs",
  "scripts/frontend_bundle.ts",
  "scripts/precompress_dist.py",
  "sdk/caic/ts",
  "tsconfig.json",
  "uv.lock",
  "vite.config.ts",
];

function fingerprint(root: string, entries: string[], skip: (entry: string) => boolean, context: string): string {
  const hash = createHash("sha256").update(context).update("\0");
  const buffer = Buffer.alloc(64 * 1024);
  function visit(entry: string): void {
    if (skip(entry)) return;
    const file = path.join(root, entry);
    const stat = fs.statSync(file, { throwIfNoEntry: false });
    if (!stat) return;
    if (stat.isDirectory()) {
      for (const name of fs.readdirSync(file).sort()) visit(`${entry}/${name}`);
      return;
    }
    const content = createHash("sha256");
    const fd = fs.openSync(file, "r");
    try {
      for (let size = fs.readSync(fd, buffer); size > 0; size = fs.readSync(fd, buffer)) {
        content.update(buffer.subarray(0, size));
      }
    } finally {
      fs.closeSync(fd);
    }
    hash.update(entry).update("\0").update(content.digest());
  }
  for (const entry of entries) visit(entry);
  return hash.digest("hex");
}

function inputFingerprint(root: string): string {
  const modeFiles = [".env", ".env.local", ".env.production", ".env.production.local"];
  const envFiles = modeFiles.map((file) => `frontend/${file}`).filter((file) => fs.existsSync(path.join(root, file)));
  const env = Object.entries(process.env)
    .filter(([key]) => key === "NODE_ENV" || key.startsWith("VITE_"))
    .sort(([a], [b]) => a.localeCompare(b));
  return fingerprint(
    root,
    [...inputs, ...envFiles],
    (entry) => entry.startsWith("frontend/src/") && /\.(?:test|bench)\.tsx?$/.test(entry),
    JSON.stringify([process.version, process.platform, process.arch, env]),
  );
}

function outputFingerprint(root: string): string {
  const output = path.join(root, dist);
  return fingerprint(output, fs.readdirSync(output).sort(), (entry) => entry === marker, "");
}

function staleReason(root: string, input: string): string | null {
  try {
    const recorded = fs.readFileSync(path.join(root, dist, marker), "utf8");
    if (!recorded.startsWith(`${input}\n`)) return "frontend build inputs changed";
    if (recorded !== `${input}\n${outputFingerprint(root)}\n`) return "frontend build output changed";
    return null;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return "frontend build fingerprint or output is missing";
    throw error;
  }
}

export function staleBundle(root: string): string | null {
  return staleReason(root, inputFingerprint(root));
}

function build(root: string): void {
  const input = inputFingerprint(root);
  if (staleReason(root, input) === null) return;
  // All three commands run in the pnpm build lifecycle with its tool paths.
  execSync(
    "node scripts/css_module_types.mjs --source-path frontend/src && vite build && " +
      "uv run --locked python scripts/precompress_dist.py --dist backend/frontend/dist --delete-originals --encodings br",
    { cwd: root, stdio: "inherit" },
  );
  if (inputFingerprint(root) !== input) throw new Error("Frontend inputs changed during the build");
  fs.writeFileSync(path.join(root, dist, marker), `${input}\n${outputFingerprint(root)}\n`);
}

if (import.meta.main) {
  const usage = "Usage: pnpm build";
  const args = process.argv.slice(2);
  if (args[0] === "-h" || args[0] === "--help") {
    console.log(usage);
  } else if (args.length !== 0) {
    console.error(usage);
    process.exitCode = 2;
  } else {
    build(path.resolve(import.meta.dirname, ".."));
  }
}
