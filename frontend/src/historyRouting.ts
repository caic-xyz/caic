// Resolves root display ownership from server-published delegation relationships and ambiguity facts.
import type { EventMessage, HistoryRoute } from "@sdk/types.gen";

export function historyOwner(event: EventMessage, routes: readonly HistoryRoute[]): string | null {
  const source = event.source;
  if (source?.agentID) return source.agentID;
  if (!source?.delegationToolUseID) return null;
  const namespace = source.rootThreadID || source.threadID || "";
  const pending = new Set(
    routes
      .filter((route) => route.toolUseID === source.delegationToolUseID && !route.agentID)
      .map((route) => route.namespace),
  );
  const candidates = new Set<string>();
  for (const route of routes) {
    if (!route.agentID || route.toolUseID !== source.delegationToolUseID) continue;
    if (namespace && route.namespace && namespace !== route.namespace) continue;
    if (!route.namespace && pending.size > 1) continue;
    candidates.add(route.agentID);
  }
  return candidates.size === 1 ? (candidates.values().next().value ?? null) : null;
}

export function displayedAtRoot(event: EventMessage, routes: readonly HistoryRoute[]): boolean {
  return (
    (event.kind === "rateLimit" && event.rateLimit?.rateLimitType !== "thread_goal") ||
    historyOwner(event, routes) === null
  );
}
