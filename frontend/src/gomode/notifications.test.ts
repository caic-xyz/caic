// Tests browser notification delivery gates independently of task-event dispatch.

import { afterEach, describe, it } from "node:test";
import { expect } from "@tests/expect";

import { notifications } from "./notifications";

const originalNotification = Object.getOwnPropertyDescriptor(window, "Notification");
const originalVisibility = Object.getOwnPropertyDescriptor(document, "visibilityState");

afterEach(() => {
  if (originalNotification) Object.defineProperty(window, "Notification", originalNotification);
  else Reflect.deleteProperty(window, "Notification");
  if (originalVisibility) Object.defineProperty(document, "visibilityState", originalVisibility);
  notifications.setVoiceActive(false);
});

describe("browser notifications", () => {
  it("delivers an alert only with permission, a hidden page, and no active voice session", () => {
    const displayed: string[] = [];
    class FakeNotification {
      static permission = "granted";
      onclose: (() => void) | null = null;
      onclick: (() => void) | null = null;
      constructor(
        title: string,
        readonly options: NotificationOptions,
      ) {
        displayed.push(`${title}:${options.tag}`);
      }
      close() {
        this.onclose?.();
      }
    }
    Object.defineProperty(window, "Notification", { configurable: true, value: FakeNotification });
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
    notifications.notify("work", "Work is ready", "work-ready", { enabled: true });
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
    notifications.notify("work", "Work is ready", "work-ready", { enabled: false });
    FakeNotification.permission = "denied";
    notifications.notify("work", "Work is ready", "work-ready", { enabled: true });
    FakeNotification.permission = "granted";
    notifications.setVoiceActive(true);
    notifications.notify("work", "Work is ready", "work-ready", { enabled: true });
    notifications.setVoiceActive(false);
    notifications.notify("work", "Work is ready", "work-ready", { enabled: true });
    expect(displayed).toEqual(["Work is ready:work-ready"]);
    notifications.dismissNotification("work");
  });
});
