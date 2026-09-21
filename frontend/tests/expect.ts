// Shared test facade: vitest's standalone expect/spy packages plus node:test fake timers,
// so unit tests run on node --test while keeping jest-style assertions.
// Loaded by every test file via `import { expect, vi } from "@tests/expect"`.
import { chai, ChaiStyleAssertions, JestAsymmetricMatchers, JestChaiExpect, JestExtend } from "@vitest/expect";
import type { ExpectStatic, MatcherState } from "@vitest/expect";
import { fn as spyFn, spyOn as spySpyOn } from "@vitest/spy";
import type { Mock, Procedure } from "@vitest/spy";
import { mock as nodeMock } from "node:test";
import { performance } from "node:perf_hooks";

// Captured before any fake timers can replace the globals: a genuine macrotask yield.
const realSetImmediate = globalThis.setImmediate;
import type { TestingLibraryMatchers } from "@testing-library/jest-dom/matchers";

chai.use(JestExtend);
chai.use(JestChaiExpect);
chai.use(ChaiStyleAssertions);
chai.use(JestAsymmetricMatchers);

export const expect: ExpectStatic = Object.assign(
  (value: unknown, message?: string) => chai.expect(value, message),
  chai.expect as unknown as Record<string, (...args: never[]) => unknown>,
) as unknown as ExpectStatic;

// Every mock created through this facade is tracked so clearAllMocks and
// restoreAllMocks can act like vitest's global reset.
const mocks = new Set<Mock<Procedure>>();
const tracked = <T>(mock: T): T => {
  mocks.add(mock as Mock<Procedure>);
  return mock;
};

const globalStubs = new Map<string, unknown>();

let timersEnabled = false;

const ensureTimers = (): void => {
  if (!timersEnabled) {
    // Start the faked clock at the real current time, like vitest's useFakeTimers.
    nodeMock.timers.enable({ apis: ["setTimeout", "setInterval", "setImmediate", "Date"], now: Date.now() });
    timersEnabled = true;
  }
};

export const vi = {
  fn: (<T extends Procedure = Procedure>(implementation?: T) => tracked(spyFn(implementation))) as {
    <T extends Procedure = Procedure>(implementation?: T): Mock<T>;
  },
  spyOn: ((...args: Parameters<typeof spySpyOn>) => tracked(spySpyOn(...args))) as unknown as typeof spySpyOn,
  stubGlobal: (name: string, value: unknown): void => {
    if (!globalStubs.has(name)) globalStubs.set(name, (globalThis as Record<string, unknown>)[name]);
    (globalThis as Record<string, unknown>)[name] = value;
  },
  unstubAllGlobals: (): void => {
    for (const [name, value] of globalStubs) (globalThis as Record<string, unknown>)[name] = value;
    globalStubs.clear();
  },
  clearAllMocks: (): void => {
    for (const mock of mocks) mock.mockClear();
  },
  resetAllMocks: (): void => {
    for (const mock of mocks) mock.mockReset();
  },
  restoreAllMocks: (): void => {
    for (const mock of mocks) mock.mockRestore();
  },
  mocked: <T extends Procedure>(mock: T): Mock<T> => mock as unknown as Mock<T>,
  useFakeTimers: (): void => {
    ensureTimers();
  },
  useRealTimers: (): void => {
    if (timersEnabled) {
      nodeMock.timers.reset();
      timersEnabled = false;
    }
  },
  setSystemTime: (time: number | Date): void => {
    ensureTimers();
    nodeMock.timers.setTime(time instanceof Date ? time.getTime() : time);
  },
  advanceTimersByTime: (milliseconds: number): void => {
    // Like vitest, advancing without fake timers enabled is a no-op.
    if (!timersEnabled) return;
    nodeMock.timers.tick(milliseconds);
  },
  advanceTimersByTimeAsync: async (milliseconds: number): Promise<void> => {
    // Like vitest, advancing without fake timers enabled is a no-op.
    if (!timersEnabled) return;
    // Yield to the event loop BEFORE ticking (sinon's tickAsync does the same), so timers
    // scheduled by pending promise continuations land inside this advancement window.
    await new Promise((resolve) => realSetImmediate(resolve));
    nodeMock.timers.tick(milliseconds);
    // Let promise continuations of fired timer callbacks settle, like vitest's async advance.
    await new Promise((resolve) => realSetImmediate(resolve));
  },
  waitFor: async <T>(callback: () => T | Promise<T>, options?: { timeout?: number; interval?: number }): Promise<T> => {
    const timeout = options?.timeout ?? 1000;
    const interval = options?.interval ?? 50;
    const start = performance.now();
    let lastError: unknown;
    for (;;) {
      try {
        return await callback();
      } catch (error) {
        lastError = error;
      }
      if (performance.now() - start >= timeout) break;
      if (timersEnabled) {
        // Fake timers are active: real sleeps would never fire, so advance them instead.
        nodeMock.timers.tick(interval);
      } else {
        await new Promise((resolve) => setTimeout(resolve, interval));
      }
      // Let promise continuations of fired callbacks settle between attempts.
      for (let flush = 0; flush < 5; flush += 1) await Promise.resolve();
    }
    throw lastError ?? new Error(`waitFor timed out after ${timeout}ms`);
  },
};

// Register jest-dom DOM matchers (toBeInTheDocument, toHaveFocus, ...) on the chai-backed expect.
// Called by setup-dom AFTER the jsdom globals are installed: importing the matchers pulls in
// @testing-library/dom, whose `screen` binds document.body at module-evaluation time.
export async function registerDomMatchers(): Promise<void> {
  const jestDomMatchers = await import("@testing-library/jest-dom/matchers");
  (chai.expect as unknown as { extend: (expect: unknown, matchers: unknown) => void }).extend(expect, jestDomMatchers);
}

declare module "@vitest/expect" {
  interface Matchers<R extends void | Promise<void> = void | Promise<void>, T = unknown> extends TestingLibraryMatchers<
    T,
    R
  > {
    // Marker so the augmented interface is not considered empty; never set at runtime.
    __jestDom?: true;
  }
}

export type { MatcherState };
