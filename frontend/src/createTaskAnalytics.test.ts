// Tests metadata-only analytics, exact-position price joins, and cancelled pricing owners.
import assert from "node:assert/strict";
import { afterEach, describe, it } from "node:test";
import { setImmediate as yieldToTasks } from "node:timers/promises";
import { createRoot, createSignal, createComponent } from "solid-js";
import type { HistoryManifest, HistoryTurnEstimate } from "@sdk/types.gen";
import { historyManifest } from "@tests/history";
import { vi } from "@tests/expect";
import type { TaskHistory } from "./taskEventTimeline";
import { createTaskAnalytics } from "./createTaskAnalytics";
import { render, waitFor } from "@solidjs/testing-library";
import StatsCharts from "./components/StatsCharts";
afterEach(() => vi.useRealTimers());
function setup(readCosts: TaskHistory["readCosts"]) {
  return createRoot((dispose) => {
    const [manifest, setManifest] = createSignal<HistoryManifest | null>(null);
    const history: TaskHistory = {
      manifest,
      messages: () => [],
      stats: () => [],
      through: () => 0,
      epoch: () => 0,
      loading: () => false,
      error: () => null,
      retry: () => {},
      position: () => null,
      readSection: async () => {
        throw new Error("Archives must stay unloaded");
      },
      readSetup: async () => [],
      readActivity: async () => [],
      readCosts,
    };
    return { ...createTaskAnalytics(history), setManifest, dispose };
  });
}
function manifestValue() {
  const value = historyManifest([], "");
  value.metadata.turns = [
    {
      recordID: 4,
      index: 2,
      sectionID: "old",
      at: "2026-01-01T00:00:00Z" as never,
      model: "original",
      durationMs: 1000,
      durationAPIMs: 900,
      usage: {
        inputTokens: 10,
        outputTokens: 2,
        cacheCreationInputTokens: 0,
        cacheReadInputTokens: 0,
        reportedModel: "",
      },
    },
  ];
  value.metadata.tools = [{ name: "Bash", calls: 2, durationMs: 2500 }];
  return value;
}
describe("server task analytics", () => {
  it("shows archived numeric facts immediately and joins zero prices by exact position", async () => {
    vi.useFakeTimers();
    let calls = 0;
    const state = setup(async () => {
      calls++;
      return [{ recordID: 4, index: 2, model: "priced", cost: { usd: 0, source: "reported" }, totalUSD: 0 }];
    });
    try {
      state.setManifest(manifestValue());
      assert.equal(state.snapshot().turns.length, 1);
      assert.equal(state.snapshot().tools[0]?.durationMs, 2500);
      assert.equal(state.snapshot().costComplete, false);
      vi.advanceTimersByTime(250);
      await yieldToTasks();
      assert.equal(calls, 1);
      assert.equal(state.snapshot().turns[0]?.model, "priced");
      assert.equal(state.snapshot().costComplete, true);
      assert.equal(state.snapshot().costUSD, 0);
    } finally {
      state.dispose();
    }
  });
  it("surfaces pricing errors and retries without transcript reads", async () => {
    vi.useFakeTimers();
    let calls = 0;
    const state = setup(async () => {
      if (++calls === 1) throw new Error("price unavailable");
      return [];
    });
    try {
      state.setManifest(manifestValue());
      vi.advanceTimersByTime(250);
      await yieldToTasks();
      assert.equal(state.error(), "price unavailable");
      state.retry();
      await yieldToTasks();
      assert.equal(state.error(), null);
      assert.equal(calls, 2);
    } finally {
      state.dispose();
    }
  });
  it("ignores a retired generation's prices", async () => {
    vi.useFakeTimers();
    let finish: (value: HistoryTurnEstimate[]) => void = () => {};
    let signal: AbortSignal | null = null;
    const state = setup((request) => {
      signal = request;
      return new Promise((resolve) => {
        finish = resolve;
      });
    });
    state.setManifest(manifestValue());
    vi.advanceTimersByTime(250);
    await yieldToTasks();
    state.setManifest(null);
    assert.equal((signal as AbortSignal | null)?.aborted, true);
    finish([{ recordID: 4, index: 2, cost: { usd: 500, source: "reported" }, totalUSD: 500 }]);
    await yieldToTasks();
    assert.equal(state.snapshot().turns.length, 0);
    assert.equal(state.snapshot().costUSD, 0);
    state.dispose();
  });
});

it("keeps charts mounted for unrelated manifests and redraws an earlier corrected turn", async () => {
  const state = setup(async () => []);
  const value = manifestValue();
  const first = value.metadata.turns[0];
  assert.ok(first);
  value.metadata.turns.push({ ...first, recordID: 8, usage: { ...first.usage } });
  state.setManifest(value);
  const view = render(() =>
    createComponent(StatsCharts, {
      stats: [],
      get turns() {
        return state.snapshot().turns;
      },
      get tools() {
        return state.snapshot().tools;
      },
    }),
  );
  try {
    const token = await view.findByLabelText("Token volume by turn");
    const tool = await view.findByLabelText("Cumulative tool time by tool kind");
    const before = state.snapshot();
    const unrelated = structuredClone(value);
    unrelated.metadata.todo = { toolUseID: "todo", todos: [] };
    state.setManifest(unrelated);
    assert.equal(state.snapshot().turns, before.turns);
    assert.equal(state.snapshot().tools, before.tools);
    assert.equal(view.getByLabelText("Token volume by turn"), token);
    assert.equal(view.getByLabelText("Cumulative tool time by tool kind"), tool);
    const corrected = structuredClone(unrelated);
    const last = before.turns[1];
    assert.ok(last);
    corrected.metadata.turns[1] = last;
    assert.ok(corrected.metadata.turns[0]);
    corrected.metadata.turns[0].usage.inputTokens = 900;
    corrected.metadata.turns[0].waitMs = 500;
    corrected.metadata.turns[0].changeStat = { files: 1, linesAdded: 2, linesDeleted: 3, binaryFiles: 0 };
    corrected.metadata.turns[0].cost = { usd: 4, source: "reported" };
    state.setManifest(corrected);
    assert.notEqual(state.snapshot().turns, before.turns);
    assert.equal(state.snapshot().turns[0]?.cost?.usd, 4);
    await waitFor(() => assert.notEqual(view.getByLabelText("Token volume by turn"), token));
    assert.equal(view.getByLabelText("Cumulative tool time by tool kind"), tool);
  } finally {
    view.unmount();
    state.dispose();
  }
});
