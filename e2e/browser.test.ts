// Tests cross-platform Chrome discovery and pinned browser selection for CI and screenshots.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import { test, type TestContext } from "node:test";
import { chromeExecutable } from "./browser";

const executable = fs.statSync(process.execPath);
const directory = fs.statSync(os.tmpdir());

function environment(t: TestContext, platform: NodeJS.Platform) {
  const descriptor = Object.getOwnPropertyDescriptor(process, "platform");
  assert.ok(descriptor);
  Object.defineProperty(process, "platform", { value: platform });
  const keys = ["CAIC_E2E_VISUALS", "CI", "LOCALAPPDATA", "PROGRAMFILES", "PROGRAMFILES(X86)"];
  const saved = keys.map((key) => process.env[key]);
  for (const key of keys) delete process.env[key];
  t.mock.method(os, "homedir", () => (platform === "win32" ? "C:\\Users\\tester" : "/Users/tester"));
  t.after(() => {
    Object.defineProperty(process, "platform", descriptor);
    keys.forEach((key, i) => {
      if (saved[i] === undefined) delete process.env[key];
      else process.env[key] = saved[i];
    });
    t.mock.restoreAll();
  });
}

for (const { platform, installed, env } of [
  { platform: "linux", installed: "/opt/google/chrome/chrome", env: {} },
  { platform: "linux", installed: "/usr/bin/google-chrome", env: {} },
  { platform: "darwin", installed: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", env: {} },
  {
    platform: "darwin",
    installed: "/Users/tester/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    env: {},
  },
  {
    platform: "win32",
    installed: "D:\\Users\\tester\\Local\\Google\\Chrome\\Application\\chrome.exe",
    env: { LOCALAPPDATA: "D:\\Users\\tester\\Local" },
  },
  {
    platform: "win32",
    installed: "D:\\Programs\\Google\\Chrome\\Application\\chrome.exe",
    env: { PROGRAMFILES: "D:\\Programs" },
  },
  {
    platform: "win32",
    installed: "D:\\Programs32\\Google\\Chrome\\Application\\chrome.exe",
    env: { "PROGRAMFILES(X86)": "D:\\Programs32" },
  },
  {
    platform: "win32",
    installed: "C:\\Users\\tester\\AppData\\Local\\Google\\Chrome\\Application\\chrome.exe",
    env: {},
  },
] satisfies { platform: NodeJS.Platform; installed: string; env: NodeJS.ProcessEnv }[]) {
  test(`discovers Chrome at ${installed}`, (t) => {
    environment(t, platform);
    Object.assign(process.env, env);
    t.mock.method(fs, "statSync", (candidate: fs.PathLike) => (candidate === installed ? executable : undefined));
    assert.equal(chromeExecutable(), installed);
  });
}

for (const env of [{ CI: "true" }, { CI: "1" }, { CAIC_E2E_VISUALS: "1" }]) {
  test(`uses bundled Chromium for ${JSON.stringify(env)} even with Chrome installed`, (t) => {
    environment(t, "linux");
    Object.assign(process.env, env);
    const lookup = t.mock.method(fs, "statSync", () => executable);
    assert.equal(chromeExecutable(), undefined);
    assert.equal(lookup.mock.callCount(), 0);
  });
}

for (const ci of ["0", "false"]) {
  test(`detects local Chrome when CI=${ci}`, (t) => {
    environment(t, "linux");
    process.env.CI = ci;
    t.mock.method(fs, "statSync", () => executable);
    assert.equal(chromeExecutable(), "/opt/google/chrome/chrome");
  });
}

test("uses bundled Chromium when Chrome is absent or a candidate is a directory", (t) => {
  environment(t, "linux");
  t.mock.method(fs, "statSync", (candidate: fs.PathLike) =>
    candidate === "/opt/google/chrome/chrome" ? directory : undefined,
  );
  assert.equal(chromeExecutable(), undefined);
});

test("surfaces filesystem errors rather than silently choosing another browser", (t) => {
  environment(t, "linux");
  const error = Object.assign(new Error("Permission denied"), { code: "EACCES" });
  t.mock.method(fs, "statSync", () => {
    throw error;
  });
  assert.throws(chromeExecutable, (actual) => actual === error);
});
