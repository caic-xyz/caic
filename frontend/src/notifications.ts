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

const activeNotifications = new Map<string, Notification>();

/**
 * Show a browser notification that an agent is waiting for input.
 * Only fires if the page is not currently visible (user tabbed away).
 */
export function notifyWaiting(taskId: string, taskName: string): void {
  if (!canNotify() || document.visibilityState === "visible") return;
  const n = new Notification(`${taskName} is ready`, {
    tag: `caic-waiting-${taskId}`,
  });
  activeNotifications.set(taskId, n);
  n.onclose = () => activeNotifications.delete(taskId);
  n.onclick = () => {
    window.focus();
    n.close();
  };
}

/**
 * Dismiss a pending notification for the given task, if any.
 * Call when the task state changes away from waiting/asking/has_plan.
 */
export function dismissNotification(taskId: string): void {
  const n = activeNotifications.get(taskId);
  if (n) {
    n.close();
    activeNotifications.delete(taskId);
  }
}
