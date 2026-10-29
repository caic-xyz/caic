// Tests recent-first history loading, bounded ordered batches, exact live handoff, resource isolation, and failure cancellation.
import assert from "node:assert/strict";
import { historyResponse } from "@tests/history";
import { setImmediate as yieldToTasks } from "node:timers/promises";
import { afterEach, describe, it } from "node:test";
import { createRoot, createEffect, createMemo, onCleanup } from "solid-js";
import { vi } from "@tests/expect";
import type { HistoryEventsHandlers } from "@sdk/api.gen";
import type { EventStats, EventResult, HistoryEvent } from "@sdk/types.gen";
import { api } from "./api";
import type { TaskHistory } from "./taskEventTimeline";
import { createTaskEventTimeline } from "./taskEventTimeline";
import { createTaskAnalytics } from "./createTaskAnalytics";
import { RootConversationProjection } from "./rootConversation";

const fetchMock = vi.spyOn(globalThis, "fetch");
const streamMock = vi.spyOn(api, "historyEvents");
afterEach(() => {
  fetchMock.mockReset();
  streamMock.mockReset();
  vi.useRealTimers();
});

function capture(events: HistoryEvent[], through: number) {
  const connections: { handlers: HistoryEventsHandlers; close: ReturnType<typeof vi.fn> }[] = [];
  streamMock.mockImplementation((_id, _cursor, handlers) => {
    const close = vi.fn();
    connections.push({ handlers, close });
    return { close } as unknown as EventSource;
  });
  fetchMock.mockImplementation(async (request, init) => {
    const url = String(request);
    init?.signal?.throwIfAborted();
    if (url.endsWith("/bootstrap"))
      return Response.json({
        projectionVersion: "v1",
        generation: "generation",
        scope: "account",
        recordsPerChunk: 256,
        through,
        cursor: `v1.generation.${through}.end`,
        resourceBase: "/history/account/v1/generation",
        inPlanMode: false,
      });
    const bounds = url.match(/\/chunks\/(\d+)\/(\d+)$/);
    assert.ok(bounds, url);
    const first = Number(bounds[1]),
      last = Number(bounds[2]);
    return historyResponse(events.filter((e) => e.recordID >= first && e.recordID <= last));
  });
  return connections;
}

function text(recordID: number, index: number, value: string): HistoryEvent {
  return { recordID, index, message: { kind: "text", ts: recordID, text: { text: value } } };
}

function input(recordID: number, value: string): HistoryEvent {
  return { recordID, index: 0, message: { kind: "userInput", ts: recordID, userInput: { text: value } } };
}

async function loaded(owner: TaskHistory) {
  for (let attempt = 0; owner.loading() && attempt < 20; attempt++) await yieldToTasks();
  assert.equal(owner.loading(), false);
}

function values(owner: TaskHistory) {
  return owner.messages().map((event) => event.text?.text ?? event.userInput?.text);
}

const sample: EventStats = {
  ts: 1,
  cpuPerc: 25,
  memPerc: 10,
  memUsed: 1,
  memLimit: 10,
  netRx: 0,
  netTx: 0,
  blockRead: 0,
  blockWrite: 0,
  diskUsed: 10,
};

describe("createTaskEventTimeline", () => {
  it("publishes one owned live view to conversation and analytics, replacing it on retry and disposal", async () => {
    vi.useFakeTimers();
    const connections = capture([input(1, "initial prompt")], 1);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    let priceCalls = 0;
    fetchMock.mockImplementation((url, init) => {
      if (!String(url).includes("/costs?")) return original(url, init);
      priceCalls++;
      return Promise.resolve(
        Response.json([
          { recordID: 3, index: 0, model: "priced", cost: { usd: 1, source: "reported" }, totalUSD: 1 },
          { recordID: 4, index: 0, model: "priced", cost: { usd: 2, source: "reported" }, totalUSD: 3 },
        ]),
      );
    });
    const lengths: number[] = [];
    const owner = createRoot((dispose) => {
      const history = createTaskEventTimeline({ taskId: () => "task" });
      const projection = new RootConversationProjection();
      let epoch = -1;
      const conversation = createMemo(() => {
        const next = history.epoch();
        const reset = next !== epoch;
        epoch = next;
        return projection.derive(history.messages(), () => true, reset);
      });
      createEffect(() => {
        lengths.push(history.messages().length);
        conversation();
      });
      onCleanup(() => projection.clear());
      return { ...history, conversation, analytics: createTaskAnalytics(history), dispose };
    });
    try {
      await loaded(owner);
      await yieldToTasks();
      const initial = owner.messages();
      const epoch = owner.epoch();
      assert.equal(priceCalls, 1);
      connections[0]?.handlers.onMessage(text(2, 0, "live response"));
      assert.equal(initial.length, 1);
      vi.advanceTimersByTime(100);
      assert.equal(owner.messages(), initial);
      assert.equal(initial.length, 2);
      assert.equal(lengths.at(-1), 2);
      assert.equal(
        owner.conversation().groups.some((group) => group.events.some((event) => event.text?.text === "live response")),
        true,
      );
      const result: EventResult = {
        subtype: "success",
        isError: false,
        result: "done",
        totalCostUSD: 0,
        duration: 1,
        durationAPI: 1,
        numTurns: 1,
        usage: {
          inputTokens: 1,
          outputTokens: 1,
          cacheCreationInputTokens: 0,
          cacheReadInputTokens: 0,
          reportedModel: "",
        },
      };
      const later = { ...result };
      connections[0]?.handlers.onMessage({ recordID: 3, index: 0, message: { kind: "result", ts: 3, result } });
      connections[0]?.handlers.onMessage({ recordID: 4, index: 0, message: { kind: "result", ts: 4, result: later } });
      assert.equal(owner.messages(), initial);
      assert.equal(owner.epoch(), epoch);
      assert.equal(lengths.at(-1), 4);
      assert.equal(owner.conversation().sessions.at(-1)?.turns.length, 2);
      assert.equal(owner.analytics.snapshot().turns.length, 2);
      assert.equal(priceCalls, 1);
      vi.advanceTimersByTime(250);
      await yieldToTasks();
      assert.equal(priceCalls, 2);
      assert.equal(owner.analytics.snapshot().costUSD, 3);
      assert.equal(owner.analytics.snapshot().costComplete, true);
      const estimates = await owner.readCosts(new AbortController().signal);
      assert.equal(estimates[0]?.result, result);
      assert.equal(estimates[1]?.result, later);
      owner.retry();
      assert.notEqual(owner.messages(), initial);
      assert.ok(owner.epoch() > epoch);
      await loaded(owner);
      connections[0]?.handlers.onMessage(input(5, "stale stream"));
      assert.equal(initial.length, 4);
      assert.deepEqual(values(owner), ["initial prompt"]);
      assert.equal(owner.analytics.snapshot().turns.length, 0);
      const replacement = owner.messages();
      owner.dispose();
      assert.notEqual(owner.messages(), replacement);
      assert.deepEqual(owner.messages(), []);
      assert.equal(connections[1]?.close.mock.calls.length, 1);
    } finally {
      owner.dispose();
    }
  });

  it("automatically loads the complete backlog without navigation, including outputless chunks", async () => {
    const connections = capture([text(2, 0, "old"), text(520, 0, "recent")], 520);
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await loaded(owner);
      assert.deepEqual(values(owner), ["old", "recent"]);
      assert.deepEqual(
        fetchMock.mock.calls.filter(([url]) => String(url).includes("/chunks/")).map(([url]) => String(url)),
        [
          "/history/account/v1/generation/chunks/513/520",
          "/history/account/v1/generation/chunks/1/256",
          "/history/account/v1/generation/chunks/257/512",
        ],
      );
      const event = input(521, "new");
      connections[0]?.handlers.onMessage(event);
      connections[0]?.handlers.onMessage(event);
      connections[0]?.handlers.onMessage({
        ...text(521, 1, "same record"),
        message: { kind: "userInput", ts: 521, userInput: { text: "same record" } },
      });
      assert.deepEqual(values(owner), ["old", "recent", "new", "same record"]);
      assert.equal(fetchMock.mock.calls.filter(([url]) => String(url).endsWith("/bootstrap")).length, 1);
    } finally {
      owner.dispose();
    }
  });

  it("shows the recent chunk and live appends while an automatically requested older chunk waits", async () => {
    const events = [text(2, 0, "old"), text(520, 0, "recent")];
    const connections = capture(events, 520);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const requested = Promise.withResolvers<void>(),
      release = Promise.withResolvers<Response>();
    fetchMock.mockImplementation((url, init) => {
      if (String(url).endsWith("/chunks/1/256")) {
        requested.resolve();
        return release.promise;
      }
      return original(url, init);
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await requested.promise;
      assert.deepEqual(values(owner), []);
      assert.deepEqual(owner.preview(), { prompt: "", response: "recent" });
      assert.equal(owner.loading(), true);
      connections[0]?.handlers.onMessage(input(521, "during backfill"));
      assert.deepEqual(values(owner), []);
      assert.deepEqual(owner.preview(), { prompt: "during backfill", response: "" });
      const before = owner.epoch();
      release.resolve(historyResponse(events.slice(0, 1)));
      await loaded(owner);
      assert.deepEqual(values(owner), ["old", "recent", "during backfill"]);
      assert.ok(owner.epoch() > before);
      connections[0]?.handlers.onMessage(input(522, "after backfill"));
      assert.deepEqual(values(owner), ["old", "recent", "during backfill", "after backfill"]);
    } finally {
      owner.dispose();
    }
  });

  it("bounds out-of-order backfill while preserving recent/live display and canonical order", async () => {
    const events = [
      text(2, 0, "first"),
      text(258, 0, "second"),
      text(514, 0, "third"),
      text(770, 0, "fourth"),
      text(1026, 0, "fifth"),
      text(1282, 0, "recent"),
    ];
    const connections = capture(events, 1282);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const release = Promise.withResolvers<Response>();
    fetchMock.mockImplementation((url, init) =>
      String(url).endsWith("/chunks/1/256") ? release.promise : original(url, init),
    );
    const owner = createRoot((dispose) => ({ dispose, ...createTaskEventTimeline({ taskId: () => "task" }) }));
    try {
      for (let i = 0; i < 20; i++) await yieldToTasks();
      const requested = fetchMock.mock.calls
        .filter(([url]) => String(url).includes("/chunks/"))
        .map(([url]) => String(url));
      assert.deepEqual(requested, [
        "/history/account/v1/generation/chunks/1281/1282",
        "/history/account/v1/generation/chunks/1/256",
        "/history/account/v1/generation/chunks/257/512",
        "/history/account/v1/generation/chunks/513/768",
        "/history/account/v1/generation/chunks/769/1024",
      ]);
      assert.equal(owner.loading(), true);
      assert.deepEqual(values(owner), []);
      assert.equal(owner.preview()?.response, "recent");
      connections[0]?.handlers.onMessage(input(1283, "live"));
      assert.equal(owner.preview()?.prompt, "live");
      release.resolve(historyResponse(events.slice(0, 1)));
      await loaded(owner);
      assert.deepEqual(values(owner), ["first", "second", "third", "fourth", "fifth", "recent", "live"]);
    } finally {
      owner.dispose();
      release.resolve(historyResponse([]));
    }
  });

  it("cancels every backfill sibling when a range fails", async () => {
    capture([text(1282, 0, "recent")], 1282);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const requests: AbortSignal[] = [];
    let cancelled = false;
    const reject = Promise.withResolvers<Response>();
    fetchMock.mockImplementation((url, init) => {
      if (!String(url).includes("/chunks/") || String(url).endsWith("/chunks/1281/1282")) return original(url, init);
      const signal = init?.signal;
      assert.ok(signal);
      requests.push(signal);
      if (String(url).endsWith("/chunks/1/256"))
        return Promise.resolve(
          new Response(
            new ReadableStream({
              start(controller) {
                controller.enqueue(new TextEncoder().encode("["));
              },
              cancel() {
                cancelled = true;
              },
            }),
          ),
        );
      if (String(url).endsWith("/chunks/257/512")) return reject.promise;
      return new Promise<Response>((_resolve, fail) =>
        signal.addEventListener("abort", () => fail(signal.reason), { once: true }),
      );
    });
    const owner = createRoot((dispose) => ({ dispose, ...createTaskEventTimeline({ taskId: () => "task" }) }));
    try {
      for (let i = 0; i < 20; i++) await yieldToTasks();
      assert.equal(requests.length, 4);
      reject.reject(new Error("range unavailable"));
      await loaded(owner);
      assert.equal(owner.error(), "range unavailable");
      assert.ok(requests.every((signal) => signal.aborted));
      await yieldToTasks();
      assert.equal(cancelled, true);
      assert.deepEqual(values(owner), []);
      assert.equal(owner.preview()?.response, "recent");
    } finally {
      owner.dispose();
      reject.resolve(historyResponse([]));
    }
  });

  it("cancels every backfill sibling on task disposal", async () => {
    capture([text(1282, 0, "recent")], 1282);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const requests: AbortSignal[] = [];
    fetchMock.mockImplementation((url, init) => {
      if (!String(url).includes("/chunks/") || String(url).endsWith("/chunks/1281/1282")) return original(url, init);
      const signal = init?.signal;
      assert.ok(signal);
      requests.push(signal);
      return new Promise<Response>((_resolve, fail) =>
        signal.addEventListener("abort", () => fail(signal.reason), { once: true }),
      );
    });
    const owner = createRoot((dispose) => ({ dispose, ...createTaskEventTimeline({ taskId: () => "task" }) }));
    try {
      for (let i = 0; i < 20; i++) await yieldToTasks();
      assert.equal(requests.length, 4);
    } finally {
      owner.dispose();
    }
    assert.ok(requests.every((signal) => signal.aborted));
    await yieldToTasks();
    assert.deepEqual(values(owner), []);
  });

  it("retains the complete active transcript from a physical record with thousands of outputs", async () => {
    const events: HistoryEvent[] = [
      {
        recordID: 1,
        index: 0,
        message: { kind: "toolUse", ts: 1, toolUse: { toolUseID: "tool", name: "Bash", input: {} } },
      },
    ];
    for (let index = 1; index <= 3000; index++)
      events.push({
        recordID: 1,
        index,
        message: { kind: "toolOutputDelta", ts: 1, toolOutputDelta: { toolUseID: "tool", delta: `output ${index}` } },
      });
    capture(events, 1);
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await loaded(owner);
      assert.equal(owner.messages().length, 3001);
      assert.equal(owner.messages()[0]?.toolUse?.toolUseID, "tool");
      assert.equal(owner.messages().at(-1)?.toolOutputDelta?.delta, "output 3000");
      assert.equal(fetchMock.mock.calls.filter(([url]) => String(url).includes("/chunks/")).length, 1);
      assert.equal(fetchMock.mock.calls.filter(([url]) => String(url).includes("/context/")).length, 0);
    } finally {
      owner.dispose();
    }
    assert.deepEqual(owner.messages(), []);
  });

  it("does not discard large SSE records arriving before the initial snapshot response", async () => {
    const connections = capture([text(1, 0, "snapshot")], 1);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const requested = Promise.withResolvers<void>(),
      release = Promise.withResolvers<Response>();
    fetchMock.mockImplementation((url, init) => {
      if (String(url).endsWith("/chunks/1/1")) {
        requested.resolve();
        return release.promise;
      }
      return original(url, init);
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await requested.promise;
      for (let index = 0; index < 2600; index++) connections[0]?.handlers.onMessage(text(2, index, `pending ${index}`));
      connections[0]?.handlers.onMessage(input(3, "after pending"));
      assert.deepEqual(owner.messages(), []);
      release.resolve(historyResponse([text(1, 0, "snapshot")]));
      await loaded(owner);
      assert.equal(owner.messages().length, 2602);
      assert.deepEqual(values(owner).slice(0, 3), ["snapshot", "pending 0", "pending 1"]);
      assert.equal(values(owner).at(-1), "after pending");
    } finally {
      owner.dispose();
    }
  });

  it("previews bounded root text and defers context-dependent events until the complete transcript", async () => {
    const old: HistoryEvent = {
      recordID: 2,
      index: 0,
      message: { kind: "toolUse", ts: 2, toolUse: { toolUseID: "tool", name: "Bash", input: {} } },
    };
    const output: HistoryEvent = {
      recordID: 520,
      index: 0,
      message: { kind: "toolOutputDelta", ts: 520, toolOutputDelta: { toolUseID: "tool", delta: "partial tool" } },
    };
    const root = text(520, 1, "prefix/" + "x".repeat(100_000));
    const child: HistoryEvent = {
      ...text(520, 2, "private child text"),
      message: {
        kind: "text",
        ts: 520,
        source: { threadID: "child", rootThreadID: "root" },
        text: { text: "private child text" },
      },
    };
    capture([old, output, root, child], 520);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const requested = Promise.withResolvers<void>(),
      release = Promise.withResolvers<Response>();
    fetchMock.mockImplementation((url, init) => {
      if (String(url).endsWith("/chunks/1/256")) {
        requested.resolve();
        return release.promise;
      }
      return original(url, init);
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await requested.promise;
      assert.deepEqual(owner.messages(), []);
      assert.equal(owner.preview()?.response, "x".repeat(16_000));
      release.resolve(historyResponse([old]));
      await loaded(owner);
      assert.equal(owner.preview(), null);
      assert.equal(owner.messages().length, 4);
      assert.equal(owner.messages()[0]?.toolUse?.toolUseID, "tool");
      assert.equal(owner.messages()[2]?.text?.text.length, 100_007);
      assert.equal(owner.messages()[3]?.text?.text, "private child text");
    } finally {
      owner.dispose();
    }
  });

  it("retries a failed automatic older chunk while retaining the recent preview until retry", async () => {
    capture([text(2, 0, "old"), text(520, 0, "recent")], 520);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    let failed = false;
    fetchMock.mockImplementation((url, init) => {
      if (!failed && String(url).endsWith("/chunks/1/256")) {
        failed = true;
        return Promise.reject(new Error("offline"));
      }
      return original(url, init);
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await loaded(owner);
      assert.equal(owner.error(), "offline");
      assert.deepEqual(values(owner), []);
      assert.equal(owner.preview()?.response, "recent");
      owner.retry();
      await loaded(owner);
      assert.equal(owner.error(), null);
      assert.deepEqual(values(owner), ["old", "recent"]);
    } finally {
      owner.dispose();
    }
  });

  it("ignores an older-range response and stream callbacks after generation replacement", async () => {
    const events = [text(2, 0, "previous prefix"), text(520, 0, "previous tail")];
    const connections = capture(events, 520);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const requested = Promise.withResolvers<void>(),
      release = Promise.withResolvers<Response>();
    let replacement = false;
    fetchMock.mockImplementation(async (url, init) => {
      if (!replacement && String(url).endsWith("/chunks/1/256")) {
        requested.resolve();
        return release.promise;
      }
      const response = await original(url, init);
      if (replacement && String(url).endsWith("/bootstrap"))
        return Response.json({
          ...(await response.json()),
          generation: "replacement",
          resourceBase: "/history/account/v1/replacement",
          cursor: "v1.replacement.520.end",
        });
      return response;
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await requested.promise;
      replacement = true;
      events.splice(0, events.length, text(2, 0, "replacement prefix"), text(520, 0, "replacement tail"));
      connections[0]?.handlers.onReset?.({ bootstrapURL: "/bootstrap" });
      await loaded(owner);
      assert.equal(connections[0]?.close.mock.calls.length, 1);
      assert.equal(connections.length, 2);
      release.resolve(historyResponse([text(2, 0, "obsolete response")]));
      connections[0]?.handlers.onMessage(input(521, "obsolete stream"));
      connections[1]?.handlers.onMessage(input(521, "current stream"));
      await yieldToTasks();
      assert.deepEqual(values(owner), ["replacement prefix", "replacement tail", "current stream"]);
    } finally {
      owner.dispose();
    }
  });

  it("releases decoded history, streams and pending responses on disposal", async () => {
    const connections = capture([text(2, 0, "old"), text(520, 0, "recent")], 520);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const requested = Promise.withResolvers<void>(),
      release = Promise.withResolvers<Response>();
    fetchMock.mockImplementation((url, init) => {
      if (String(url).endsWith("/chunks/1/256")) {
        requested.resolve();
        return release.promise;
      }
      return original(url, init);
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    await requested.promise;
    assert.deepEqual(owner.messages(), []);
    assert.equal(owner.preview()?.response, "recent");
    owner.dispose();
    assert.equal(connections[0]?.close.mock.calls.length, 1);
    release.resolve(historyResponse([text(2, 0, "obsolete")]));
    connections[0]?.handlers.onMessage(input(521, "obsolete stream"));
    await yieldToTasks();
    assert.deepEqual(owner.messages(), []);
  });

  it("merges independent resource samples by timestamp and bounds only the sample ring", async () => {
    const connections = capture([text(2, 0, "recent")], 2);
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await loaded(owner);
      connections[0]?.handlers.onStats?.({ ...sample, diskUsed: 20 });
      connections[0]?.handlers.onStatsSnapshot?.({ samples: [{ ...sample, diskUsed: 30 }] });
      connections[0]?.handlers.onStats?.({ ...sample, diskUsed: 20 });
      await yieldToTasks();
      assert.equal(owner.stats()[0]?.diskUsed, 20);
      for (let ts = 2; ts <= 65; ts++) connections[0]?.handlers.onStats?.({ ...sample, ts });
      assert.equal(owner.stats().length, 60);
      assert.equal(owner.stats()[0]?.ts, 6);
      assert.deepEqual(values(owner), ["recent"]);
    } finally {
      owner.dispose();
    }
  });

  it("joins initial and live prices by exact captured result position and aborts replaced owners", async () => {
    const root = {
      subtype: "success",
      isError: false,
      result: "root",
      totalCostUSD: 0,
      duration: 1,
      durationAPI: 1,
      numTurns: 1,
      usage: {
        inputTokens: 1,
        outputTokens: 1,
        cacheCreationInputTokens: 0,
        cacheReadInputTokens: 0,
        reportedModel: "",
      },
    };
    const child = { ...root, result: "child" };
    const connections = capture(
      [
        { recordID: 2, index: 0, message: { kind: "result", ts: 2, result: child, source: { agentID: "child" } } },
        { recordID: 2, index: 1, message: { kind: "result", ts: 2, result: root } },
      ],
      2,
    );
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    let delayed = Promise.withResolvers<Response>();
    let priceSignal: AbortSignal | null = null;
    let pending = false;
    fetchMock.mockImplementation((url, init) => {
      if (!String(url).includes("/costs?")) return original(url, init);
      priceSignal = init?.signal ?? null;
      return pending
        ? delayed.promise.then((response) => response.clone())
        : Promise.resolve(
            Response.json([
              { recordID: 2, index: 0, model: "child", cost: { usd: 99, source: "reported" }, totalUSD: 99 },
              { recordID: 2, index: 1, model: "rerouted", cost: { usd: 0, source: "reported" }, totalUSD: 0 },
            ]),
          );
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await loaded(owner);
      const costs = await owner.readCosts(new AbortController().signal);
      assert.equal(costs.length, 1);
      assert.equal(costs[0]?.result, owner.messages()[1]?.result);
      assert.equal(costs[0]?.model, "rerouted");
      assert.equal(costs[0]?.cost?.usd, 0);
      const live = { ...root, result: "live root" };
      const later = { ...root, result: "later output from the same raw record" };
      connections[0]?.handlers.onMessage({
        recordID: 3,
        index: 0,
        message: { kind: "result", ts: 3, result: child, source: { agentID: "child" } },
      });
      connections[0]?.handlers.onMessage({ recordID: 3, index: 1, message: { kind: "result", ts: 3, result: live } });
      pending = true;
      const updating = owner.readCosts(new AbortController().signal);
      connections[0]?.handlers.onMessage({ recordID: 3, index: 2, message: { kind: "result", ts: 3, result: later } });
      delayed.resolve(
        Response.json([
          { recordID: 2, index: 1, model: "original", totalUSD: 0 },
          { recordID: 3, index: 0, model: "child", totalUSD: 99 },
          { recordID: 3, index: 1, model: "live", totalUSD: 1 },
          { recordID: 3, index: 2, model: "later", totalUSD: 2 },
        ]),
      );
      const updates = await updating;
      assert.deepEqual(
        updates.map((value) => value.result),
        [costs[0]?.result, live],
      );
      assert.equal(updates[0]?.result, costs[0]?.result);
      assert.equal(updates[1]?.result, owner.messages()[3]?.result);
      const refreshed = await owner.readCosts(new AbortController().signal);
      assert.deepEqual(
        refreshed.map((value) => value.result),
        [costs[0]?.result, live, later],
      );
      delayed = Promise.withResolvers<Response>();
      const caller = new AbortController();
      const cancelled = owner.readCosts(caller.signal);
      caller.abort();
      assert.equal((priceSignal as AbortSignal | null)?.aborted, true);
      delayed.resolve(Response.json([]));
      await assert.rejects(cancelled, { name: "AbortError" });
      delayed = Promise.withResolvers<Response>();
      const stale = owner.readCosts(new AbortController().signal);
      owner.retry();
      assert.equal((priceSignal as AbortSignal | null)?.aborted, true);
      delayed.resolve(Response.json([]));
      await assert.rejects(stale, { name: "AbortError" });
    } finally {
      owner.dispose();
    }
  });

  it("repairs resource rings on reconnect and exposes decoder failures through shared recovery", async () => {
    const connections = capture([text(2, 0, "history")], 2);
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await loaded(owner);
      const handlers = connections[0]?.handlers;
      assert.ok(handlers);
      handlers.onStatsSnapshot?.({ samples: [sample] });
      handlers.onStats?.({ ...sample, ts: 2, diskUsed: 20 });
      handlers.onStatsSnapshot?.({ samples: [{ ...sample, ts: 3, diskUsed: 30 }] });
      assert.deepEqual(
        owner.stats().map((value) => value.ts),
        [3],
      );
      handlers.onError(new Error("Invalid HistoryStats.samples"));
      assert.equal(owner.error(), "Invalid HistoryStats.samples");
      assert.equal(connections[0]?.close.mock.calls.length, 1);
      owner.retry();
      await loaded(owner);
      connections[1]?.handlers.onStatsSnapshot?.({ samples: [{ ...sample, ts: 4 }] });
      assert.equal(owner.error(), null);
      assert.deepEqual(
        owner.stats().map((value) => value.ts),
        [4],
      );
      assert.deepEqual(values(owner), ["history"]);
    } finally {
      owner.dispose();
    }
  });

  it("rediscovers a stale immutable URL and completes the replacement backlog", async () => {
    capture([text(2, 0, "old"), text(520, 0, "recent")], 520);
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    let stale = false;
    fetchMock.mockImplementation(async (url, init) => {
      if (!stale && String(url).endsWith("/chunks/1/256")) {
        stale = true;
        return new Response("stale", { status: 409 });
      }
      const response = await original(url, init);
      if (stale && String(url).endsWith("/bootstrap"))
        return Response.json({
          ...(await response.json()),
          generation: "replacement",
          resourceBase: "/history/account/v1/replacement",
          cursor: "v1.replacement.520.end",
        });
      return response;
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await loaded(owner);
      assert.deepEqual(values(owner), ["old", "recent"]);
      assert.equal(owner.error(), null);
      assert.equal(fetchMock.mock.calls.filter(([url]) => String(url).endsWith("/bootstrap")).length, 2);
      assert.ok(fetchMock.mock.calls.some(([url]) => String(url).includes("/replacement/chunks/1/256")));
    } finally {
      owner.dispose();
    }
  });

  it("accepts live output for an initially empty task and retries named history failures", async () => {
    const connections = capture([], 0);
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task" }),
    }));
    try {
      await loaded(owner);
      connections[0]?.handlers.onMessage(input(1, "first input"));
      assert.deepEqual(values(owner), ["first input"]);
      connections[0]?.handlers.onHistoryError?.({ message: "replay failed" });
      assert.equal(owner.error(), "replay failed");
      assert.equal(connections[0]?.close.mock.calls.length, 1);
      owner.retry();
      await loaded(owner);
      assert.equal(connections.length, 2);
      assert.equal(owner.error(), null);
    } finally {
      owner.dispose();
    }
  });
});
