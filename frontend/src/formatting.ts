// Shared formatting utilities for caic web UI values.

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

export function formatBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value >= 10 || unit === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[unit]}`;
}

export function formatTime(timestamp: string): string {
  const date = new Date(timestamp);
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleString();
}

// Formats an elapsed duration given in milliseconds.
//
// Call formatElapsed(seconds * 1000) for API durations.
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
  if (ratio >= 0.9) return "var(--color-danger)";
  if (ratio >= 0.75) return "var(--color-warning-text)";
  return "inherit";
}

export function stateColor(state: TaskState): string {
  switch (state) {
    case "running":
    case "branching":
    case "provisioning":
    case "starting":
    case "pending":
    case "pulling":
    case "pushing":
      return "var(--color-state-active-bg)";
    case "asking":
      return "var(--color-state-asking-bg)";
    case "has_plan":
      return "var(--color-state-plan-bg)";
    case "crashed":
      return "var(--color-state-crashed-bg)";
    case "failed":
      return "var(--color-state-failed-bg)";
    case "purging":
    case "stopping":
      return "var(--color-state-stopping-bg)";
    case "purged":
      return "var(--color-state-purged-bg)";
    case "stopped":
      return "var(--color-state-stopped-bg)";
    case "waiting":
      return "var(--color-state-waiting-bg)";
  }
}

/** True when the prompt cache has expired. Returns false when cacheExpiresAt is unset (no data from backend). */
export function isCacheStale(nowMs: number, cacheExpiresAt?: string): boolean {
  if (!cacheExpiresAt) return false;
  const expiresMs = new Date(cacheExpiresAt).getTime();
  return expiresMs > 0 && nowMs > expiresMs;
}

/** Returns a redder variant of the state color when the cache is stale. */
export function staleStateColor(state: TaskState): string {
  return `color-mix(in srgb, ${stateColor(state)} 75%, var(--color-danger))`;
}

function pathFromInput(input: Record<string, unknown>): string {
  const path = input.file_path ?? input.path;
  return typeof path === "string" ? path : "";
}

function basename(path: string): string {
  return path.replace(/^.*\//, "");
}

export function toolCallDetail(name: string, input: Record<string, unknown>): string {
  switch (name.toLowerCase()) {
    case "read":
    case "write":
      return basename(pathFromInput(input));
    case "edit":
      return basename(pathFromInput(input));
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
      return typeof input.description === "string" ? input.description : "";
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

