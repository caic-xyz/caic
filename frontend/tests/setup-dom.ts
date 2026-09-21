// Installs jsdom globals, the Solid TSX transform, and asset stubs before tests run.
// Loaded via `node --test --import ./frontend/tests/setup-dom.ts` so every test-file child
// process gets a DOM before module evaluation, like vitest's environment: "jsdom".
import { readFileSync } from "node:fs";
import { registerHooks } from "node:module";
import { fileURLToPath } from "node:url";
import babel from "@babel/core";
import solidPreset from "babel-preset-solid";
import typescriptPreset from "@babel/preset-typescript";
import { JSDOM } from "jsdom";
import { afterEach } from "node:test";

// Load the assertion facade FIRST, before the jsdom globals are installed below: chai 6
// subclasses the global `Event` at module-evaluation time for its plugin events, so it must
// capture Node's native Event. Everything evaluated after the globals block (components,
// tests) sees jsdom's Event, matching vitest's jsdom environment.
await import("@tests/expect");

const dom = new JSDOM('<!doctype html><html><body id="root"></body></html>', {
  url: "http://localhost:3000/",
  pretendToBeVisual: true,
});

const globals = dom.window as unknown as Record<string, unknown>;
const globalTarget = globalThis as unknown as Record<string, unknown>;
for (const key of [
  "document",
  "HTMLElement",
  "HTMLInputElement",
  "HTMLSelectElement",
  "HTMLTextAreaElement",
  "HTMLHeadElement",
  "HTMLBodyElement",
  "HTMLHtmlElement",
  "HTMLAnchorElement",
  "HTMLButtonElement",
  "HTMLIFrameElement",
  "SVGElement",
  "Element",
  "Node",
  "DOMParser",
  "XMLSerializer",
  "NodeFilter",
  "DOMRect",
  "DOMRectReadOnly",
  "Event",
  "EventTarget",
  "CustomEvent",
  "KeyboardEvent",
  "MouseEvent",
  "getComputedStyle",
  "requestAnimationFrame",
  "cancelAnimationFrame",
  "localStorage",
  "sessionStorage",
]) {
  const value = globals[key];
  if (value !== undefined) globalTarget[key] = value;
}
globalTarget["window"] = dom.window;
Object.defineProperty(globalTarget, "navigator", { value: dom.window.navigator, configurable: true });

// Delegate the timer family from the jsdom window to globalThis so node:test's fake
// timers (which replace the globals) govern `window.setTimeout` calls too. Late-bound:
// reads the current global at call time so fake-timer swaps are honored.
for (const timer of [
  "setTimeout",
  "setInterval",
  "clearTimeout",
  "clearInterval",
  "setImmediate",
  "clearImmediate",
  "queueMicrotask",
]) {
  if (globalTarget[timer] !== undefined) {
    Object.defineProperty(dom.window, timer, {
      configurable: true,
      value: (...args: unknown[]) =>
        (globalThis as unknown as Record<string, (...args: unknown[]) => unknown>)[timer](...args),
    });
  }
}

// jsdom does not implement window.matchMedia; stub it so components that call
// it (e.g. for touch-device detection) don't throw in tests.
const matchMediaStub = (query: string) => ({
  matches: false,
  media: query,
  onchange: null,
  addListener: () => {},
  removeListener: () => {},
  addEventListener: () => {},
  removeEventListener: () => {},
  dispatchEvent: () => false,
});
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: matchMediaStub });
globalTarget["matchMedia"] = matchMediaStub;

// jsdom does not implement ResizeObserver; stub it so components using it
// (e.g. VoiceOverlay measuring its panel height) don't throw in tests.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalTarget["ResizeObserver"] = ResizeObserverStub as unknown as typeof ResizeObserver;

// jsdom does not implement window scrolling, and components scroll on mount;
// stub it as a no-op.
globalTarget["scrollTo"] = () => {};
dom.window.scrollTo = () => {};

// jsdom does not implement element scrolling; components may use it to keep a
// selected item visible without needing browser layout in unit tests.
const HTMLElementCtor = globalTarget["HTMLElement"] as unknown as { prototype: Record<string, unknown> };
if (!HTMLElementCtor.prototype.scrollIntoView) {
  Object.defineProperty(HTMLElementCtor.prototype, "scrollIntoView", {
    configurable: true,
    value: () => {},
  });
}

// jsdom does not implement HTMLDialogElement.showModal / close; stub them so
// components using native <dialog> (CloneRepoDialog, CameraCapture) don't
// throw when calling showModal in onMount.
const dialogCtor = Object.getPrototypeOf(dom.window.document.createElement("dialog")) as {
  constructor: unknown;
  prototype?: undefined;
} & Record<string, unknown>;
if (dialogCtor && !dialogCtor.showModal) {
  dialogCtor.showModal = function showModal(this: HTMLDialogElement) {
    this.setAttribute("open", "");
  };
  dialogCtor.close = function close(this: HTMLDialogElement) {
    this.removeAttribute("open");
    this.dispatchEvent(new Event("close"));
  };
}

// Now that the DOM exists, register jest-dom matchers (imports @testing-library/dom, whose
// `screen` binds document.body at module-evaluation time) and the testing-library cleanup hook.
const { registerDomMatchers } = await import("@tests/expect");
await registerDomMatchers();
const { cleanup } = await import("@solidjs/testing-library");
afterEach(cleanup);

// CSS modules, SVG components (?solid via vite-solid-svg), ?url assets, and ?raw text
// imports are build-time transforms the node runner does not perform; stub them here.
const STUB_SUFFIXES = [".css", ".png", ".jpg", ".jpeg", ".woff", ".woff2"];

const isStubbedSpecifier = (specifier: string): boolean => {
  const path = specifier.split("?")[0]!;
  if (specifier.includes("?url") || specifier.includes("?solid") || specifier.includes("?raw")) return true;
  return STUB_SUFFIXES.some((suffix) => path.endsWith(suffix));
};

// Solid TSX must go through babel-preset-solid (the vite-plugin-solid transform);
// esbuild cannot compile Solid's JSX, and solid-js ships no jsx-runtime functions.
registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier.includes("?raw")) {
      // ?raw keeps its real file: resolve the stripped specifier, then mark the URL so
      // the load hook below can serve the file's text as the module's default export.
      const resolved = nextResolve(specifier.split("?")[0]!, context);
      return { url: `${resolved.url}?raw`, shortCircuit: true };
    }
    if (isStubbedSpecifier(specifier)) {
      return { url: `test-asset:${specifier}`, shortCircuit: true };
    }
    return nextResolve(specifier, context);
  },
  load(url, context, nextLoad) {
    if (url.startsWith("test-asset:")) {
      const specifier = url.slice("test-asset:".length);
      if (specifier.includes("?solid")) {
        // Solid components expect a component function returning a DOM node; forward the
        // props (data-testid, class, ...) as attributes so queries keep working.
        return {
          format: "module",
          shortCircuit: true,
          source:
            `export default (props) => {\n` +
            `  const el = globalThis.document.createElement("span");\n` +
            `  for (const key of Object.keys(props)) {\n` +
            `    if (key !== "children") el.setAttribute(key, String(props[key]));\n` +
            `  }\n` +
            `  return el;\n` +
            `};`,
        };
      }
      if (specifier.includes("?url")) {
        return { format: "module", shortCircuit: true, source: `export default ${JSON.stringify(specifier)};` };
      }
      if (specifier.endsWith(".css")) {
        // CSS modules: class names are scoped per module with a stable hash, mirroring
        // vitest's default so substring class assertions stay exact.
        let hash = 5381;
        for (const ch of specifier) hash = ((hash << 5) + hash + ch.charCodeAt(0)) >>> 0;
        return {
          format: "module",
          shortCircuit: true,
          source: `const styles = new Proxy({}, { get: (_target, key) => typeof key === "string" ? \`_\${key}_${hash.toString(36)}\` : key }); export default styles;`,
        };
      }
      // Images and fonts: a stable URL string.
      return { format: "module", shortCircuit: true, source: `export default ${JSON.stringify(specifier)};` };
    }
    if (url.endsWith("?raw")) {
      const filePath = fileURLToPath(url.slice(0, -"?raw".length));
      return {
        format: "module",
        shortCircuit: true,
        source: `export default ${JSON.stringify(readFileSync(filePath, "utf8"))};`,
      };
    }
    if (/\.tsx$/.test(url.split("?")[0]!)) {
      const filePath = fileURLToPath(url.split("?")[0]!);
      const result = babel.transformSync(readFileSync(filePath, "utf8"), {
        filename: filePath,
        sourceType: "module",
        presets: [
          [solidPreset, { moduleName: "solid-js/web", generate: "dom", hydratable: false }],
          [typescriptPreset, { isTSX: true, allExtensions: true }],
        ],
      });
      return { format: "module", shortCircuit: true, source: result?.code ?? "" };
    }
    return nextLoad(url, context);
  },
});
