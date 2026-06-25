// Global test setup for vitest with SolidJS testing-library.

import "@testing-library/jest-dom/vitest";

// jsdom does not implement window.matchMedia; stub it so components that call
// it (e.g. for touch-device detection) don't throw in tests.
Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }),
});

// jsdom does not implement ResizeObserver; stub it so components using it
// (e.g. VoiceOverlay measuring its panel height) don't throw in tests.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver = ResizeObserverStub as unknown as typeof ResizeObserver;

// jsdom logs "not implemented" warnings for scrollTo; tests only need it to
// exist for components that restore scroll position during route changes.
Object.defineProperty(window, "scrollTo", {
  writable: true,
  value: () => {},
});

// jsdom does not implement HTMLDialogElement.showModal / close; stub them so
// components using native <dialog> (CloneRepoDialog, CameraCapture) don't
// throw when calling showModal in onMount.
if (!HTMLDialogElement.prototype.showModal) {
  HTMLDialogElement.prototype.showModal = function showModal() {
    this.setAttribute("open", "");
  };
  HTMLDialogElement.prototype.close = function close() {
    this.removeAttribute("open");
    this.dispatchEvent(new Event("close"));
  };
}
