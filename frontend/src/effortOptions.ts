// Valid thinking-effort levels per harness, shared by the new-task form and fork dialog.
import type { Harness } from "@sdk/types.gen";

/**
 * Returns the list of valid effort levels for a given harness.
 * Empty for harnesses that don't support thinking effort.
 */
export function effortOptions(harness: Harness): string[] {
  switch (harness) {
    case "claude":
      return ["low", "medium", "high", "max"];
    case "codex":
      return ["none", "minimal", "low", "medium", "high", "xhigh"];
    case "pi":
      return ["off", "minimal", "low", "medium", "high", "xhigh"];
    case "kilo":
    case "opencode":
      return [];
  }
  // Exhaustive: harness may be an empty/unset signal value at render time.
  return [];
}
