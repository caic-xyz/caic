// Selects installed Chrome for local tests and installs bundled Chromium when required.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

// Setup and test configuration share this decision. Use the detected executable
// directly so per-user installations work too. A broken installation must fail
// at launch rather than silently change the browser under test.
export function chromeExecutable(): string | undefined {
  if (
    (process.env.CI && process.env.CI !== "0" && process.env.CI !== "false") ||
    process.env.CAIC_E2E_VISUALS === "1"
  ) {
    return undefined;
  }
  let candidates: string[];
  switch (process.platform) {
    case "darwin":
      candidates = [
        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
        path.join(os.homedir(), "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
      ];
      break;
    case "linux":
      candidates = [
        "/opt/google/chrome/chrome",
        "/usr/bin/google-chrome",
        "/usr/bin/google-chrome-stable",
        "/usr/local/bin/google-chrome",
        "/usr/local/bin/google-chrome-stable",
      ];
      break;
    case "win32":
      candidates = [
        process.env.LOCALAPPDATA || path.win32.join(os.homedir(), "AppData", "Local"),
        process.env.PROGRAMFILES || "C:\\Program Files",
        process.env["PROGRAMFILES(X86)"] || "C:\\Program Files (x86)",
      ].map((root) => path.win32.join(root, "Google", "Chrome", "Application", "chrome.exe"));
      break;
    default:
      return undefined;
  }
  return candidates.find((candidate) => fs.statSync(candidate, { throwIfNoEntry: false })?.isFile());
}

if (process.argv[1] && pathToFileURL(path.resolve(process.argv[1])).href === import.meta.url) {
  const args = process.argv.slice(2);
  if (args.length === 1 && (args[0] === "--help" || args[0] === "-h")) {
    process.stdout.write(
      "Usage: tsx e2e/browser.ts --install\nDetect Chrome or install bundled Chromium's headless shell.\n",
    );
  } else if (args.length === 1 && args[0] === "--install") {
    const executable = chromeExecutable();
    if (executable) {
      process.stdout.write(`Using installed Chrome: ${executable}\n`);
    } else {
      process.stdout.write("Using bundled Chromium headless shell\n");
      const require = createRequire(import.meta.url);
      execFileSync(process.execPath, [require.resolve("@playwright/test/cli"), "install", "chromium", "--only-shell"], {
        stdio: "inherit",
      });
    }
  } else {
    process.stderr.write("Usage: tsx e2e/browser.ts --install\n");
    process.exitCode = 1;
  }
}
