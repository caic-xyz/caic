// Reports Node test failures and diagnostics, with compact progress only on CI.
import { inspect } from "node:util";

const showProgress = process.env["CI"] !== undefined;

function formatFailure(error) {
  return inspect(error.cause ?? error, { colors: false, depth: null });
}

export default async function* quietTestReporter(source) {
  for await (const event of source) {
    if (event.type === "test:diagnostic" && event.data.level !== "info") {
      yield `\n${event.data.level}: ${event.data.message}\n`;
    }
    if (event.type === "test:fail") {
      yield `X\n✖ ${event.data.name}\n${formatFailure(event.data.details.error)}\n`;
    }
    if (event.type === "test:pass" && showProgress) {
      yield ".";
    }
    if (event.type === "test:stderr") {
      yield `\n${event.data.message}`;
    }
    if (event.type === "test:coverage") {
      yield formatCoverage(event.data.summary);
    }
    if (event.type === "test:summary" && showProgress) {
      yield "\n";
    }
  }
}

function formatCoverage(summary) {
  const files = (summary?.files ?? []).map((file) => ({
    path: file.path.replace(`${summary.workingDirectory}/`, ""),
    lines: file.coveredLinePercent,
  }));
  if (files.length === 0) return "";
  const rows = [...files];
  if (typeof summary.totals?.coveredLinePercent === "number") {
    rows.push({ path: "all files", lines: summary.totals.coveredLinePercent });
  }
  const width = Math.max(...rows.map((row) => row.path.length));
  let out = `\nLine coverage\n  ${"file".padEnd(width)}  lines%\n`;
  for (const row of rows) out += `  ${row.path.padEnd(width)}  ${row.lines.toFixed(1)}\n`;
  return `${out}\n`;
}
