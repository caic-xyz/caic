// Runs frontend benchmarks on tinybench (the engine vitest bench used) without vitest.
// Usage: tsx scripts/bench.ts [--save=<file>] [--compare=<file>] <glob> [<glob>...]
//   --save    writes the results as a JSON baseline for later comparison
//   --compare prints per-case deltas against a saved baseline (new cases are noted,
//             missing cases are reported; exit code 1 never happens — deltas are informational)
import { glob } from "node:fs/promises";
import { readFileSync, writeFileSync } from "node:fs";
import { performance } from "node:perf_hooks";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { Bench } from "tinybench";

import { takeCases, type BenchCase } from "../frontend/tests/bench";

interface BaselineEntry {
  opsSec: number;
  meanMs: number;
  p75Ms: number;
  p99Ms: number;
}
type Baseline = Record<string, BaselineEntry>;

const args = process.argv.slice(2);
const savePath = args.find((arg) => arg.startsWith("--save="))?.slice("--save=".length);
const comparePath = args.find((arg) => arg.startsWith("--compare="))?.slice("--compare=".length);
const patterns = args.filter((arg) => !arg.startsWith("--"));

const cases: BenchCase[] = [];
for (const pattern of patterns) {
  for await (const file of glob(pattern)) {
    await import(pathToFileURL(resolve(file)).href);
    for (const entry of takeCases()) {
      if (!entry.file) entry.file = file;
      cases.push(entry);
    }
  }
}
if (cases.length === 0) {
  console.error(`No bench cases found for: ${patterns.join(", ")}`);
  process.exit(1);
}

const baseline: Baseline = comparePath ? (JSON.parse(readFileSync(comparePath, "utf8")) as Baseline) : {};

const results: Baseline = {};
const width = Math.max(10, ...cases.map((entry) => entry.name.length));
console.log(
  `  ${"name".padEnd(width)}    ${"ops/sec".padStart(9)}      avg      p75      p99       rme  samples  file`,
);

for (const entry of cases) {
  const bench = new Bench({ time: entry.time, warmupTime: entry.warmupTime, now: () => performance.now() });
  bench.add(entry.name, entry.fn);
  await bench.run();
  const result = bench.tasks[0]?.result;
  if (!result) throw new Error(`bench produced no result for ${entry.name}`);
  const opsSec = result.throughput.mean;
  results[entry.name] = { opsSec, meanMs: result.latency.mean, p75Ms: result.latency.p75, p99Ms: result.latency.p99 };
  const base = baseline[entry.name];
  const delta = base
    ? `  (${(opsSec / base.opsSec - 1) * 100 >= 0 ? "+" : ""}${((opsSec / base.opsSec - 1) * 100).toFixed(1)}% vs baseline)`
    : "";
  console.log(
    `* ${entry.name.padEnd(width)}  ${opsSec.toFixed(2).padStart(11)}  ${result.latency.mean.toFixed(4).padStart(8)}  ${result.latency.p75.toFixed(4).padStart(8)}  ${result.latency.p99.toFixed(4).padStart(8)}  ±${(result.latency.rme * 100).toFixed(2)}%`.padEnd(
      width + 60,
    ) + `  ${result.latency.samplesCount}  ${entry.file}${delta}`,
  );
}

if (savePath) {
  writeFileSync(savePath, `${JSON.stringify(results, null, 2)}\n`);
  console.log(`\nSaved baseline to ${savePath}`);
}

if (comparePath) {
  const missing = Object.keys(baseline).filter((name) => !(name in results));
  if (missing.length > 0) console.log(`\nMissing from this run (present in baseline): ${missing.join(", ")}`);
}
