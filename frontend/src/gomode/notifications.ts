// Generic browser notification permission, delivery, dismissal, and voice suppression.

interface NotificationOptions {
  enabled: boolean;
}

/** Request notification permission if not already granted. */
function requestNotificationPermission(options: NotificationOptions): void {
  if (!options.enabled) return;
  if ("Notification" in window && window.Notification.permission === "default") {
    window.Notification.requestPermission();
  }
}

/** Returns true when we're allowed to send notifications. */
function canNotify(options: NotificationOptions): boolean {
  return options.enabled && "Notification" in window && window.Notification.permission === "granted";
}

const activeNotifications = new Map<string, Notification>();

let voiceActive = false;

/** Set whether the voice agent is active. Suppresses browser notifications while true. */
function setVoiceActive(active: boolean): void {
  voiceActive = active;
}

/**
 * Show a browser notification that an agent is waiting for input.
 * Only fires if the page is not currently visible (user tabbed away).
 */
function notify(id: string, title: string, tag: string, options: NotificationOptions): void {
  if (!canNotify(options) || document.visibilityState === "visible" || voiceActive) return;
  dismissNotification(id);
  const n = new window.Notification(title, { tag });
  activeNotifications.set(id, n);
  n.onclose = () => {
    if (activeNotifications.get(id) === n) activeNotifications.delete(id);
  };
  n.onclick = () => {
    window.focus();
    n.close();
  };
}

/**
 * Dismiss a pending notification by its service-supplied identity.
 */
function dismissNotification(id: string): void {
  const n = activeNotifications.get(id);
  if (n) {
    n.close();
    activeNotifications.delete(id);
  }
}

/** Notification operations as one object so tests can spy on them. */
export const notifications = {
  requestNotificationPermission,
  notify,
  dismissNotification,
  setVoiceActive,
};
