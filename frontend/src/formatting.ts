// Shared formatting utilities for caic web UI values.
// Note: formatElapsed takes milliseconds (JS timestamps). Call
// formatElapsed(seconds * 1000) for API durations.
import type { TaskState } from "@sdk/types.gen";

/** Returns the currency symbol for a currency code. Unknown codes return "??". */
export function currencySign(currency: string): string {
  if (currency === "CNY") return "¥";
  if (currency === "USD") return "$";
  return "??";
}

/** Formats a monetary balance value. */
export function formatBalance(currency: string, total: number): string {
  return `${currencySign(currency)}${total.toFixed(2)}`;
}

export function formatCost(usd: number): string {
  return usd < 0.01 ? "<$0.01" : `$${usd.toFixed(2)}`;
}

export function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}Mt`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(0)}kt`;
  return `${n}t`;
}

export function formatDuration(seconds: number): string {
  if (seconds < 1) return `${Math.round(seconds * 1000)}ms`;
  return `${seconds.toFixed(1)}s`;
}

// Formats an elapsed duration given in milliseconds.
export function formatElapsed(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m}m ${s % 60}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h}h ${m % 60}m` : `${h}h`;
}

export function tokenColor(current: number, limit: number): string {
  if (limit <= 0) return "inherit";
  const ratio = current / limit;
  if (ratio >= 0.9) return "#dc3545";
  if (ratio >= 0.75) return "#d4a017";
  return "inherit";
}

export function stateColor(state: TaskState): string {
  switch (state) {
    case "running":
    case "branching":
    case "provisioning":
    case "starting":
      return "#d4edda";
    case "asking":
      return "#cce5ff";
    case "has_plan":
      return "#ede9fe";
    case "crashed":
      return "#ffe0d6";
    case "failed":
      return "#f8d7da";
    case "purging":
    case "stopping":
      return "#fde2c8";
    case "purged":
      return "#e2e3e5";
    case "stopped":
      return "#c8daf0";
    case "pending":
    case "waiting":
    case "pulling":
    case "pushing":
      return "#fff3cd";
  }
}

/** True when the prompt cache has expired. Returns false when cacheExpiresAt is unset (no data from backend). */
export function isCacheStale(nowMs: number, cacheExpiresAt?: string): boolean {
  if (!cacheExpiresAt) return false;
  const expiresMs = new Date(cacheExpiresAt).getTime();
  return expiresMs > 0 && nowMs > expiresMs;
}

/** Blend a hex color toward a target hex by `amount` (0–1). */
function blendHex(hex: string, target: string, amount: number): string {
  const ch = (s: string, i: number) => parseInt(s.slice(i, i + 2), 16);
  const mix = (a: number, t: number) => Math.round(a + (t - a) * amount);
  const r = mix(ch(hex, 1), ch(target, 1));
  const g = mix(ch(hex, 3), ch(target, 3));
  const bl = mix(ch(hex, 5), ch(target, 5));
  return `#${r.toString(16).padStart(2, "0")}${g.toString(16).padStart(2, "0")}${bl.toString(16).padStart(2, "0")}`;
}

/** Returns a redder variant of the state color when the cache is stale. */
export function staleStateColor(state: TaskState): string {
  return blendHex(stateColor(state), "#dc3545", 0.25);
}

export function toolCallDetail(name: string, input: Record<string, unknown>): string {
  switch (name.toLowerCase()) {
    case "read":
    case "write":
      return typeof input.file_path === "string" ? input.file_path.replace(/^.*\//, "") : "";
    case "edit":
      return typeof input.file_path === "string" ? input.file_path.replace(/^.*\//, "") : "";
    case "bash":
      return typeof input.command === "string" ? input.command.trimStart() : "";
    case "grep":
      return typeof input.pattern === "string" ? input.pattern : "";
    case "glob":
      return typeof input.pattern === "string" ? input.pattern : "";
    case "task":
      return typeof input.description === "string" ? input.description : "";
    case "agent":
    case "subagent":
      return subagentDetail(input);
    case "webfetch":
      return typeof input.url === "string" ? input.url : "";
    case "websearch":
      return typeof input.query === "string" ? input.query : "";
    case "notebookedit":
      return typeof input.notebook_path === "string" ? input.notebook_path.replace(/^.*\//, "") : "";
    default:
      return "";
  }
}

// One subagent invocation parsed from a Pi `subagent` tool call's arguments.
export interface SubagentSpawn {
  agent: string;
  task: string;
  label?: string;
  phase?: string;
}

// Structured view of a `subagent` tool call: the orchestration kind and the
// subagents it spawns. Mirrors the backend parse in backend/internal/agent/pi.
export interface SubagentInfo {
  kind: "single" | "parallel" | "chain" | "action" | "";
  action?: string;
  spawns: SubagentSpawn[];
}

interface SubagentStep {
  agent?: unknown;
  task?: unknown;
  label?: unknown;
  phase?: unknown;
}

function toStep(s: SubagentStep): SubagentSpawn | null {
  if (typeof s.agent !== "string" || !s.agent) return null;
  return {
    agent: s.agent,
    task: typeof s.task === "string" ? s.task : "",
    label: typeof s.label === "string" ? s.label : undefined,
    phase: typeof s.phase === "string" ? s.phase : undefined,
  };
}

// parseSubagentInput decodes a `subagent` tool call's input into an ordered list
// of spawned subagents. Handles single, parallel-batch (tasks[]), and chain
// (chain[].parallel[]) shapes; introspection calls (list/status) spawn none.
export function parseSubagentInput(input: Record<string, unknown>): SubagentInfo {
  const spawns: SubagentSpawn[] = [];
  const push = (s: SubagentStep | undefined) => {
    if (!s) return;
    const spawn = toStep(s);
    if (spawn) spawns.push(spawn);
  };

  const chain = input.chain;
  const tasks = input.tasks;
  if (Array.isArray(chain)) {
    for (const step of chain as Array<Record<string, unknown>>) {
      if (Array.isArray(step?.parallel)) {
        for (const s of step.parallel as SubagentStep[]) push(s);
      } else {
        push(step as SubagentStep);
      }
    }
    return { kind: spawns.length ? "chain" : "", spawns };
  }
  if (Array.isArray(tasks)) {
    for (const s of tasks as SubagentStep[]) push(s);
    return { kind: spawns.length ? "parallel" : "", spawns };
  }
  push(input as SubagentStep);
  if (spawns.length) return { kind: "single", spawns };
  const action = typeof input.action === "string" ? input.action : undefined;
  return { kind: action ? "action" : "", action, spawns };
}

// subagentDetail summarises a subagent tool call for the tool-card header,
// e.g. "reviewer — Review the last commit" or "chain · reviewer ×3, worker".
export function subagentDetail(input: Record<string, unknown>): string {
  const { kind, action, spawns } = parseSubagentInput(input);
  if (spawns.length === 0) return action ?? "";
  if (spawns.length === 1) {
    const s = spawns[0];
    const detail = s.label || s.task.split("\n").map((l) => l.trim()).find((l) => l) || "";
    return detail ? `${s.agent} — ${detail}` : s.agent;
  }
  const order: string[] = [];
  const counts = new Map<string, number>();
  for (const s of spawns) {
    if (!counts.has(s.agent)) order.push(s.agent);
    counts.set(s.agent, (counts.get(s.agent) ?? 0) + 1);
  }
  const parts = order.map((a) => {
    const n = counts.get(a) ?? 0;
    return n > 1 ? `${a} ×${n}` : a;
  });
  return `${kind} · ${parts.join(", ")}`;
}
