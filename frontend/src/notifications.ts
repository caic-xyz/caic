// Browser notification helpers for alerting when agents need attention.

/** Request notification permission if not already granted. */
export function requestNotificationPermission(): void {
  if ("Notification" in window && Notification.permission === "default") {
    Notification.requestPermission();
  }
}

/** Returns true when we're allowed to send notifications. */
function canNotify(): boolean {
  return "Notification" in window && Notification.permission === "granted";
}

/**
 * Show a browser notification that an agent is waiting for input.
 * Only fires if the page is not currently visible (user tabbed away).
 */
export function notifyWaiting(taskId: string, taskName: string): void {
  if (!canNotify() || document.visibilityState === "visible") return;
  const n = new Notification("Agent waiting for input", {
    body: `Task #${taskId}: ${taskName}`,
    tag: `caic-waiting-${taskId}`,
  });
  n.onclick = () => {
    window.focus();
    n.close();
  };
}
