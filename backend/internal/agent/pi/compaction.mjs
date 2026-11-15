// Runs configured Pi RPC sessions with in-memory native compaction budgets and normal CLI resources.
import { Buffer } from "node:buffer";
import console from "node:console";
import process from "node:process";
import { accessSync, closeSync, constants, existsSync, openSync, readFileSync, readSync, realpathSync } from "node:fs";
import { dirname, join, delimiter } from "node:path";
import { pathToFileURL } from "node:url";
import { parseArgs } from "node:util";
import { setTimeout, clearTimeout } from "node:timers";

const usage =
  "Usage: node compaction.mjs [--reserve-tokens N] [--keep-recent-tokens N] [--session ID] [--extension PATH]";

// npm uses executable symlinks; pnpm's documented cmd-shim-target marker names
// the same entrypoint. Resolve from the selected pi executable, never a second
// global package installation which could differ from the CLI used by caic.
function packageRoot() {
  for (const dir of (process.env.PATH ?? "").split(delimiter)) {
    const candidate = join(dir, "pi");
    try {
      accessSync(candidate, constants.X_OK);
    } catch {
      continue;
    }
    let entry = realpathSync(candidate);
    const fd = openSync(entry, "r");
    const buffer = Buffer.alloc(65536);
    let prefix;
    try {
      prefix = buffer.subarray(0, readSync(fd, buffer)).toString();
    } finally {
      closeSync(fd);
    }
    const target = prefix.match(/^# cmd-shim-target=(.+)$/m);
    if (target) entry = realpathSync(target[1]);
    for (let root = dirname(entry); ; root = dirname(root)) {
      const manifest = join(root, "package.json");
      if (existsSync(manifest)) {
        const pkg = JSON.parse(readFileSync(manifest, "utf8"));
        if (pkg.name === "@earendil-works/pi-coding-agent") return root;
      }
      if (dirname(root) === root) break;
    }
    throw new Error("The selected pi executable does not resolve to an installed Pi SDK package");
  }
  throw new Error("pi executable is missing from PATH");
}

async function main() {
  const { values } = parseArgs({
    options: {
      "reserve-tokens": { type: "string" },
      "keep-recent-tokens": { type: "string" },
      session: { type: "string" },
      extension: { type: "string", multiple: true },
      help: { type: "boolean", short: "h" },
    },
  });
  if (values.help) {
    console.log(usage);
    return;
  }
  const overrides = {};
  for (const [flag, key] of [
    ["reserve-tokens", "reserveTokens"],
    ["keep-recent-tokens", "keepRecentTokens"],
  ]) {
    if (values[flag] === undefined) continue;
    const n = Number(values[flag]);
    if (!Number.isSafeInteger(n) || n < 0 || n > 2147483647)
      throw new Error(`${flag} must be between 0 and 2147483647`);
    overrides[key] = n;
  }
  if (Object.keys(overrides).length === 0) throw new Error("At least one compaction budget is required");
  const root = packageRoot();
  const sdk = await import(pathToFileURL(join(root, "dist/index.js")).href);
  // Pi 1.5's public SDK owns settings, session storage, resources, and RPC.
  // Its CLI registers built-in codemode/tool-search/MCP factories from this
  // native module. Retain those factories instead of silently losing CLI tools.
  // https://github.com/earendil-works/pi/blob/v1.5.0/packages/coding-agent/src/main.ts
  const { builtInExtensions } = await import(pathToFileURL(join(root, "dist/extensions/index.js")).href);
  if (typeof sdk.createAgentSessionRuntime !== "function" || !Array.isArray(builtInExtensions)) {
    throw new Error("Configured Pi compaction requires the Pi 1.5 SDK runtime and CLI extensions");
  }
  // These are the same native startup helpers used by Pi's CLI. In particular,
  // the dispatcher owns proxy environment support, timeout policy, and its
  // stream-termination error listener; keep those out of caic's implementation.
  const { applyHttpProxySettings, configureHttpDispatcher } = await import(
    pathToFileURL(join(root, "dist/core/http-dispatcher.js")).href
  );
  const { takeOverStdout } = await import(pathToFileURL(join(root, "dist/core/output-guard.js")).href);
  const { resolveModelScopeWithDiagnostics } = await import(
    pathToFileURL(join(root, "dist/core/model-resolver.js")).href
  );
  const { runMigrations } = await import(pathToFileURL(join(root, "dist/migrations.js")).href);
  const { ENV_SESSION_DIR, expandTildePath } = await import(pathToFileURL(join(root, "dist/config.js")).href);
  const { collectSettingsDiagnostics, deduplicateDiagnostics } = await import(
    pathToFileURL(join(root, "dist/core/settings-diagnostics.js")).href
  );
  const { formatNoModelsAvailableMessage } = await import(pathToFileURL(join(root, "dist/core/auth-guidance.js")).href);
  takeOverStdout();
  process.env.PI_CODING_AGENT = "true";
  process.env.AI_AGENT = "pi";
  const startupDiagnostics = [];
  const checkSettings = (manager) => startupDiagnostics.push(...collectSettingsDiagnostics(manager));
  const cwd = process.cwd();
  const agentDir = sdk.getAgentDir();
  const bootstrap = sdk.SettingsManager.create(cwd, agentDir, { projectTrusted: false });
  checkSettings(bootstrap);
  applyHttpProxySettings(bootstrap.getGlobalSettings().httpProxy);
  configureHttpDispatcher();
  runMigrations(cwd);
  const createRuntime = async (target) => {
    const settingsManager = sdk.SettingsManager.create(target.cwd, target.agentDir, { projectTrusted: true });
    checkSettings(settingsManager);
    // Native resource reloads refresh disk settings. Reapply the captured task
    // overrides as an inline extension factory on every load/reload, before
    // the session can run a turn. No settings are written back to disk.
    const applyBudgets = () => {
      const modelOverrides = {};
      for (const [key, value] of Object.entries(settingsManager.getSettings().compaction?.modelOverrides ?? {})) {
        modelOverrides[key] = { ...value, ...overrides };
      }
      settingsManager.applyOverrides({ compaction: { ...overrides, modelOverrides } });
    };
    const services = await sdk.createAgentSessionServices({
      cwd: target.cwd,
      agentDir: target.agentDir,
      settingsManager,
      modelRuntimeSignal: globalThis.AbortSignal.timeout(15000),
      resourceLoaderOptions: {
        extensionFactories: [...builtInExtensions, applyBudgets],
        additionalExtensionPaths: values.extension ?? [],
      },
    });
    checkSettings(settingsManager);
    const diagnostics = [
      ...services.diagnostics,
      ...services.resourceLoader
        .getExtensions()
        .errors.map(({ path, error }) => ({ type: "error", message: `Failed to load extension "${path}": ${error}` })),
      ...(services.resourceLoader.getExtensions().warnings ?? []).map(({ path, warning }) => ({
        type: "warning",
        message: `Extension package "${path}": ${warning}`,
      })),
    ];
    const patterns = settingsManager.getEnabledModels();
    const scope = patterns?.length
      ? await resolveModelScopeWithDiagnostics(patterns, services.modelRuntime, {
          signal: globalThis.AbortSignal.timeout(15000),
        })
      : { scopedModels: [], diagnostics: [] };
    diagnostics.push(...scope.diagnostics);
    let selected;
    if (target.sessionManager.buildSessionContext().messages.length === 0 && scope.scopedModels.length > 0) {
      const savedProvider = settingsManager.getDefaultProvider();
      const savedModel = settingsManager.getDefaultModel();
      selected =
        scope.scopedModels.find(({ model }) => model.provider === savedProvider && model.id === savedModel) ??
        scope.scopedModels[0];
    }
    const created = await sdk.createAgentSessionFromServices({
      services,
      sessionManager: target.sessionManager,
      sessionStartEvent: target.sessionStartEvent,
      scopedModels: scope.scopedModels,
      model: selected?.model,
      thinkingLevel: selected?.thinkingLevel,
    });
    return { ...created, services, diagnostics };
  };
  const settings = sdk.SettingsManager.create(cwd, agentDir, { projectTrusted: true });
  checkSettings(settings);
  const envSessionDir = process.env[ENV_SESSION_DIR];
  const sessionDir = (envSessionDir ? expandTildePath(envSessionDir) : undefined) ?? settings.getSessionDir();
  let sessionManager;
  if (values.session) {
    const path = sdk.SessionManager.findById(cwd, values.session, sessionDir);
    if (!path) throw new Error(`Pi session ${values.session} was not found in this workspace`);
    sessionManager = sdk.SessionManager.open(path, sessionDir, cwd);
  } else {
    sessionManager = sdk.SessionManager.create(cwd, sessionDir);
  }
  const runtime = await sdk.createAgentSessionRuntime(createRuntime, { cwd, agentDir, sessionManager });
  applyHttpProxySettings(runtime.services.settingsManager.getGlobalSettings().httpProxy);
  configureHttpDispatcher(runtime.services.settingsManager.getHttpIdleTimeoutMs());
  for (const diagnostic of deduplicateDiagnostics([...startupDiagnostics, ...runtime.diagnostics])) {
    const prefix = diagnostic.type === "error" ? "Error: " : diagnostic.type === "warning" ? "Warning: " : "";
    console.error(`${prefix}${diagnostic.message}`);
  }
  if (runtime.diagnostics.some((diagnostic) => diagnostic.type === "error")) {
    process.exit(1);
  }
  if (!runtime.session.model) {
    console.error(formatNoModelsAvailableMessage());
    process.exit(1);
  }
  const offline = ["1", "true", "yes"].includes(process.env.PI_OFFLINE?.toLowerCase());
  if (!offline) {
    const controller = new globalThis.AbortController();
    const timeout = setTimeout(() => controller.abort(), 15000);
    void runtime.services.modelRuntime
      .refresh({ signal: controller.signal })
      .catch(() => {})
      .finally(() => clearTimeout(timeout));
  }
  await sdk.runRpcMode(runtime);
}

main().catch((err) => {
  console.error(`${err.message}\n${usage}`);
  process.exitCode = 1;
});
