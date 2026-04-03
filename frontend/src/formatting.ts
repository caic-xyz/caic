// Shared formatting utilities, parallel to android/util/Formatting.kt.
// Note: formatElapsed takes milliseconds (JS timestamps); the Android
// equivalent takes seconds. Call formatElapsed(seconds * 1000) for API durations.

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
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function tokenColor(current: number, limit: number): string {
  if (limit <= 0) return "inherit";
  const ratio = current / limit;
  if (ratio >= 0.9) return "#dc3545";
  if (ratio >= 0.75) return "#d4a017";
  return "inherit";
}

export function stateColor(state: string): string {
  switch (state) {
    case "running":
      return "#d4edda";
    case "asking":
      return "#cce5ff";
    case "has_plan":
      return "#ede9fe";
    case "failed":
      return "#f8d7da";
    case "purging":
      return "#fde2c8";
    case "stopping":
      return "#fde2c8";
    case "purged":
      return "#e2e3e5";
    case "stopped":
      return "#c8daf0";
    default:
      return "#fff3cd";
  }
}

const STALE_THRESHOLD_MS = 3_600_000; // 1 hour

/** True when the last state change is older than 1 hour. */
export function isCacheStale(stateUpdatedAtSec: number, nowMs: number): boolean {
  return stateUpdatedAtSec > 0 && nowMs - stateUpdatedAtSec * 1000 > STALE_THRESHOLD_MS;
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
export function staleStateColor(state: string): string {
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
      if (typeof input.command === "string") {
        const cmd = input.command.trimStart();
        return cmd.length > 60 ? cmd.slice(0, 57) + "..." : cmd;
      }
      return "";
    case "grep":
      return typeof input.pattern === "string" ? input.pattern : "";
    case "glob":
      return typeof input.pattern === "string" ? input.pattern : "";
    case "task":
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
