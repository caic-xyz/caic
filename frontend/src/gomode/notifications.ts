// Browser notification helpers for alerting when agents need attention.

interface NotificationOptions {
  enabled: boolean;
}

/** Request notification permission if not already granted. */
export function requestNotificationPermission(options: NotificationOptions): void {
  if (!options.enabled) return;
  if ("Notification" in window && Notification.permission === "default") {
    Notification.requestPermission();
  }
}

/** Returns true when we're allowed to send notifications. */
function canNotify(options: NotificationOptions): boolean {
  return options.enabled && "Notification" in window && Notification.permission === "granted";
}

const activeNotifications = new Map<string, Notification>();

let voiceActive = false;

/** Set whether the voice agent is active. Suppresses browser notifications while true. */
export function setVoiceActive(active: boolean): void {
  voiceActive = active;
}

/**
 * Show a browser notification that an agent is waiting for input.
 * Only fires if the page is not currently visible (user tabbed away).
 */
export function notifyWaiting(taskId: string, taskName: string, options: NotificationOptions): void {
  showNotification(taskId, `${taskName} is ready`, `caic-waiting-${taskId}`, options);
}

/** Show a service-supplied notification title for a task. */
export function notifyServiceEvent(taskId: string, title: string, options: NotificationOptions): void {
  showNotification(taskId, title, `caic-event-${taskId}`, options);
}

function showNotification(taskId: string, title: string, tag: string, options: NotificationOptions): void {
  if (!canNotify(options) || document.visibilityState === "visible" || voiceActive) return;
  dismissNotification(taskId);
  const n = new Notification(title, { tag });
  activeNotifications.set(taskId, n);
  n.onclose = () => {
    if (activeNotifications.get(taskId) === n) activeNotifications.delete(taskId);
  };
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
