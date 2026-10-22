// Owns URL-backed task subview presentation and weak focus restoration for workspace navigation.

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

export function taskViewMode(pathname: string, search: string): TaskViewMode {
  const query = new URLSearchParams(search);
  const match = /^\/task\/[^/]+\/(diff|info|processes|stats|view|vnc)$/.exec(pathname);
  if (!match || (match[1] === "view" && !query.get("path") && !query.get("attachment"))) return "closed";
  return query.get("fullscreen") === "1" ? "fullscreen" : "docked";
}
