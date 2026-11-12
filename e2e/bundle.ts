// Detects a built frontend bundle older than its inputs, which the e2e server would serve stale.
import fs from "node:fs";
import path from "node:path";

// Inputs Vite bundles into backend/frontend/dist (see vite.config.ts); test
// files are not bundled.
const bundleInputs = [
  "frontend/index.html",
  "frontend/public",
  "frontend/src",
  "package.json",
  "sdk/caic/ts",
  "vite.config.ts",
];
// Every build empties dist before writing it, so this file's modification time
// is the build time even when the output bytes are unchanged.
export const bundleStamp = "backend/frontend/dist/index.html.br";

/**
 * Returns why the bundle under root cannot contain its inputs, or null when it
 * is current. A checkout that touches inputs also reports a stale bundle until
 * the next build.
 */
export function staleBundle(root: string): string | null {
  let built: number;
  try {
    built = fs.statSync(path.join(root, bundleStamp)).mtimeMs;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return `${bundleStamp} is missing`;
    throw err;
  }
  for (const input of bundleInputs) {
    const changed = newerThan(root, input, built);
    if (changed !== null) return `${changed} changed after the last frontend build`;
  }
  return null;
}

function newerThan(root: string, relative: string, built: number): string | null {
  const stat = fs.statSync(path.join(root, relative));
  if (!stat.isDirectory()) return /\.test\.tsx?$/.test(relative) || stat.mtimeMs <= built ? null : relative;
  for (const entry of fs.readdirSync(path.join(root, relative))) {
    const changed = newerThan(root, path.join(relative, entry), built);
    if (changed !== null) return changed;
  }
  return null;
}
