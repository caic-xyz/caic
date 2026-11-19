// Silently checks initial JS/CSS budgets and reports measurements on request or failure.
//
// Measure a frontend build: node scripts/startup_budget.mjs --dist backend/frontend/dist --report
// make build runs this same check in CI. It never rebuilds or changes the bundle.
// Scope: Vite-managed assets from the single index.html application entry.
// Build-only .vite/manifest.json.br metadata is untracked and excluded from go:embed and serving.
// The manifest owns import parsing, so static re-exports and shared CSS count;
// dynamic-only chunks, fonts, images, and other non-JS/CSS assets do not.
// Budgets leave about 5% above the current payload while protecting most of the
// saving from deferring optional routes. Change a budget only with a measured,
// intentional startup tradeoff, not by copying a new build's byte count.

import { readFileSync, statSync } from "node:fs";
import { resolve, relative, isAbsolute } from "node:path";
import { fileURLToPath } from "node:url";
import { brotliDecompressSync } from "node:zlib";

const usage = "Usage: node scripts/startup_budget.mjs --dist <built-directory> [--report]";
const limits = {
  JavaScript: { brotli: 165_000, decoded: 630_000 },
  CSS: { brotli: 23_500, decoded: 135_000 },
};
// Build metadata and individual initial chunks must be small. Refuse malformed
// or unexpectedly huge input rather than allocate unbounded decompression data.
const maxBodyBytes = 8 * 1024 * 1024;

function readBody(dist, file) {
  const path = resolve(dist, `${file}.br`);
  const local = relative(resolve(dist), path);
  if (!local || isAbsolute(local) || local.startsWith("..") || file.includes("\\")) {
    throw new Error(`Asset escapes the build directory: ${file}`);
  }
  if (statSync(path).size > maxBodyBytes) throw new Error(`Compressed asset exceeds 8 MiB: ${file}`);
  const compressed = readFileSync(path);
  const decoded = brotliDecompressSync(compressed, { maxOutputLength: maxBodyBytes });
  return { file, brotli: compressed.length, decoded: decoded.length, body: decoded };
}

function strings(value, label) {
  if (value === undefined) return [];
  if (!Array.isArray(value) || value.some((item) => typeof item !== "string" || item === "")) {
    throw new Error(`Invalid ${label}: expected an array of asset names`);
  }
  return value;
}

/** Follow exactly the manifest entry's eager imports, counting each served body once. */
export function measureStartup(dist) {
  const manifest = JSON.parse(readBody(dist, ".vite/manifest.json").body.toString());
  if (!manifest || Array.isArray(manifest) || typeof manifest !== "object") {
    throw new Error("Invalid startup manifest: expected Vite's manifest object");
  }
  if (manifest["index.html"]?.isEntry !== true) {
    throw new Error("Missing index.html entry in startup manifest");
  }
  const visited = new Set();
  const scripts = new Set();
  const styles = new Set();
  const pending = ["index.html"];
  while (pending.length) {
    const key = pending.pop();
    if (visited.has(key)) continue;
    visited.add(key);
    const chunk = manifest[key];
    if (!chunk || typeof chunk !== "object" || typeof chunk.file !== "string") {
      throw new Error(`Missing or invalid static manifest chunk: ${key}`);
    }
    if (chunk.file.endsWith(".js") || chunk.file.endsWith(".mjs")) scripts.add(chunk.file);
    else if (chunk.file.endsWith(".css") && key !== "index.html") styles.add(chunk.file);
    else throw new Error(`Unsupported initial chunk type: ${chunk.file}`);
    for (const file of strings(chunk.css, `CSS dependencies of ${key}`)) {
      if (!file.endsWith(".css")) throw new Error(`Invalid CSS asset: ${file}`);
      styles.add(file);
    }
    pending.push(...strings(chunk.imports, `static imports of ${key}`));
  }
  return [
    { name: "JavaScript", files: [...scripts].sort() },
    { name: "CSS", files: [...styles].sort() },
  ].map(({ name, files }) => {
    const assets = files.map((file) => {
      const result = readBody(dist, file);
      return { file, brotli: result.brotli, decoded: result.decoded };
    });
    return {
      name,
      assets,
      brotli: assets.reduce((total, asset) => total + asset.brotli, 0),
      decoded: assets.reduce((total, asset) => total + asset.decoded, 0),
    };
  });
}

function main(rawArgs) {
  const report = rawArgs.includes("--report");
  const args = rawArgs.filter((arg) => arg !== "--report");
  if (args.length === 1 && (args[0] === "--help" || args[0] === "-h")) {
    process.stdout.write(`${usage}\nChecks silently on success; --report prints initial JS/CSS measurements.\n`);
    return 0;
  }
  let argumentError = "";
  if (args.length === 0 || (args[0] === "--dist" && (args.length === 1 || args[1] === "" || args[1].startsWith("-")))) {
    argumentError = "Missing required --dist <built-directory>.";
  } else if (args[0] !== "--dist") {
    argumentError = `Unknown argument: ${args[0]}.`;
  } else if (args.length !== 2) {
    argumentError = `Unexpected argument: ${args[2]}.`;
  }
  if (argumentError) {
    process.stderr.write(`${argumentError}\n${usage}\n`);
    return 2;
  }
  let exceeded = false;
  const lines = [];
  const measured = measureStartup(args[1]);
  for (const group of measured) {
    for (const encoding of ["brotli", "decoded"]) {
      const budget = limits[group.name][encoding];
      const remaining = budget - group[encoding];
      lines.push(
        `${group.name} ${encoding}: ${group[encoding]} / ${budget} bytes (${Math.abs(remaining)} ${remaining < 0 ? "over budget" : "remaining"})`,
      );
      if (remaining < 0) exceeded = true;
    }
  }
  if (report || exceeded) {
    (exceeded ? process.stderr : process.stdout).write(`${lines.join("\n")}\n`);
  }
  if (exceeded) {
    process.stderr.write(
      "Startup budget exceeded. Defer optional code or justify an intentional budget change. Largest initial assets:\n",
    );
    for (const asset of measured
      .flatMap((group) => group.assets)
      .sort((a, b) => b.brotli - a.brotli)
      .slice(0, 8)) {
      process.stderr.write(`  ${asset.file}: ${asset.brotli} Brotli / ${asset.decoded} decoded bytes\n`);
    }
  }
  return exceeded ? 1 : 0;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    process.exitCode = main(process.argv.slice(2));
  } catch (error) {
    process.stderr.write(`Startup measurement failed: ${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  }
}
