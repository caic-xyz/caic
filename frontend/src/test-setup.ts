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
