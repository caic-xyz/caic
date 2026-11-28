// Owns URL-backed task presentation, Escape handling, and weak focus restoration for workspace navigation.

let viewOrigin: WeakRef<HTMLAnchorElement> | null = null;

export function rememberTaskViewOrigin(link: HTMLAnchorElement) {
  viewOrigin = new WeakRef(link);
}

export function restoreTaskViewOrigin() {
  const origin = viewOrigin?.deref();
  viewOrigin = null;
  const target =
    origin?.isConnected && origin.getClientRects().length > 0
      ? origin
      : document.querySelector<HTMLElement>("[data-testid='conversation-pane']");
  target?.focus({ preventScroll: true });
}

export type TaskViewMode = "closed" | "docked" | "fullscreen";

export function isTaskDetailPath(pathname: string): boolean {
  return /^\/task\/[^/]+\/?$/.test(pathname);
}

export function taskDetailFullscreen(pathname: string, search: string): boolean {
  return isTaskDetailPath(pathname) && new URLSearchParams(search).get("fullscreen") === "1";
}

export function taskFullscreen(pathname: string, search: string): boolean {
  return taskDetailFullscreen(pathname, search) || taskViewMode(pathname, search) === "fullscreen";
}

export function isTaskViewEscape(event: KeyboardEvent): boolean {
  if (event.key !== "Escape" || event.defaultPrevented || event.isComposing || event.repeat) return false;
  const target = event.target instanceof Element ? event.target : null;
  return !document.querySelector("dialog[open]") && !target?.closest('[role="dialog"], [role="menu"]');
}

export function taskViewMode(pathname: string, search: string): TaskViewMode {
  const query = new URLSearchParams(search);
  const match = /^\/task\/[^/]+\/(diff|info|perf(?:\/(?:turns|contexts)\/[^/]+)?|processes|view|vnc)$/.exec(pathname);
  if (!match || (match[1] === "view" && !query.get("path") && !query.get("attachment") && !query.get("image")))
    return "closed";
  return query.get("fullscreen") === "1" ? "fullscreen" : "docked";
}
