// Tests shared-history numeric updates, lazy pricing, resets, cancellation, and pricing-only retries.
import assert from "node:assert/strict";
import { setImmediate as yieldToTasks } from "node:timers/promises";
import { afterEach, describe, it } from "node:test";
import { batch, createRoot, createSignal } from "solid-js";
import type { EventMessage, EventResult } from "@sdk/types.gen";
import type { TaskHistory } from "./taskEventTimeline";
import type { ResultEstimate } from "./taskAnalytics";
import { vi } from "@tests/expect";
import { createTaskAnalytics } from "./createTaskAnalytics";

afterEach(() => vi.useRealTimers());

const result: EventResult = {
  subtype: "success",
  isError: false,
  result: "body",
  totalCostUSD: 0,
  duration: 1,
  durationAPI: 1,
  numTurns: 1,
  usage: { inputTokens: 10, outputTokens: 5, cacheCreationInputTokens: 0, cacheReadInputTokens: 0, reportedModel: "" },
};
function setup(readCosts: TaskHistory["readCosts"]) {
  return createRoot((dispose) => {
    const [messages, setMessages] = createSignal<EventMessage[]>([]);
    const [epoch, setEpoch] = createSignal(0);
    const [loading, setLoading] = createSignal(true);
    const history: TaskHistory = {
      messages,
      epoch,
      loading,
      preview: () => null,
      stats: () => [],
      error: () => null,
      retry: () => {},
      readCosts,
    };
    return { ...createTaskAnalytics(history), dispose, setMessages, setEpoch, setLoading };
  });
}
describe("shared task analytics", () => {
  it("waits for history, publishes only numeric changes, and joins zero costs by result identity", async () => {
    let calls = 0;
    const state = setup(async () => {
      calls++;
      return [{ result, model: "rerouted", cost: { usd: 0, source: "reported" } }];
    });
    await yieldToTasks();
    assert.equal(calls, 0);
    state.setMessages([{ kind: "result", ts: 1, result }]);
    state.setLoading(false);
    await yieldToTasks();
    assert.equal(calls, 1);
    assert.equal(state.snapshot().turns[0]?.model, "rerouted");
    assert.equal(state.snapshot().costComplete, true);
    const before = state.snapshot();
    state.setMessages((events) => [...events, { kind: "textDelta", ts: 2, textDelta: { text: "more" } }]);
    await yieldToTasks();
    assert.equal(state.snapshot(), before);
    assert.equal(calls, 1);
    state.dispose();
  });
  it("keeps pricing errors independent and retries without replacing history", async () => {
    let calls = 0;
    const state = setup(async () => {
      if (++calls === 1) throw new Error("pricing failed");
      return [];
    });
    state.setMessages([{ kind: "result", ts: 1, result }]);
    state.setLoading(false);
    await yieldToTasks();
    assert.equal(state.error(), "pricing failed");
    state.retry();
    await yieldToTasks();
    assert.equal(state.error(), null);
    assert.equal(state.snapshot().turns.length, 1);
    assert.equal(calls, 2);
    state.dispose();
  });
  it("coalesces new completed turns and cancels queued pricing when hidden", async () => {
    vi.useFakeTimers();
    let calls = 0;
    const state = setup(async () => {
      calls++;
      return [];
    });
    state.setLoading(false);
    await yieldToTasks();
    assert.equal(calls, 1);
    state.setMessages([{ kind: "result", ts: 1, result: { ...result } }]);
    vi.advanceTimersByTime(100);
    state.setMessages((events) => [...events, { kind: "result", ts: 2, result: { ...result } }]);
    vi.advanceTimersByTime(249);
    assert.equal(calls, 1);
    vi.advanceTimersByTime(1);
    await yieldToTasks();
    assert.equal(calls, 2);
    assert.equal(state.snapshot().turns.length, 2);
    state.setMessages((events) => [...events, { kind: "result", ts: 3, result: { ...result } }]);
    state.retry();
    await yieldToTasks();
    assert.equal(calls, 3);
    vi.advanceTimersByTime(1000);
    assert.equal(calls, 3);
    state.setMessages((events) => [...events, { kind: "result", ts: 4, result: { ...result } }]);
    state.dispose();
    vi.advanceTimersByTime(1000);
    assert.equal(calls, 3);
  });

  it("aborts stale prices on reset and disposal", async () => {
    const signals: AbortSignal[] = [];
    let finish: (value: readonly ResultEstimate[]) => void = () => {};
    const state = setup((signal) => {
      signals.push(signal);
      return new Promise((resolve) => {
        finish = resolve;
      });
    });
    state.setMessages([{ kind: "result", ts: 1, result }]);
    state.setLoading(false);
    await yieldToTasks();
    const stale = finish;
    batch(() => {
      state.setEpoch(1);
      state.setMessages([]);
    });
    await yieldToTasks();
    assert.equal(signals[0]?.aborted, true);
    stale([{ result, model: "stale", cost: { usd: 99, source: "estimated" } }]);
    await yieldToTasks();
    assert.equal(state.snapshot().turns.length, 0);
    state.dispose();
    assert.equal(signals.at(-1)?.aborted, true);
  });
});
