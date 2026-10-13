// Owns URL-backed document presentation and weak focus restoration for file navigation.

let fileOrigin: WeakRef<HTMLAnchorElement> | null = null;

export function rememberFileOrigin(link: HTMLAnchorElement) {
  fileOrigin = new WeakRef(link);
}

export function restoreFileOrigin() {
  const origin = fileOrigin?.deref();
  fileOrigin = null;
  const target =
    origin?.isConnected && origin.getClientRects().length > 0
      ? origin
      : document.querySelector<HTMLElement>("[data-testid='conversation-pane']");
  target?.focus({ preventScroll: true });
}

export type FileViewMode = "closed" | "docked" | "fullscreen";

export function fileViewMode(pathname: string, search: string): FileViewMode {
  const query = new URLSearchParams(search);
  if (!/^\/task\/[^/]+\/view$/.test(pathname) || !query.get("path")) return "closed";
  return query.get("fullscreen") === "1" ? "fullscreen" : "docked";
}
