// Tests automatic recent-first cached backlog loading, exact live handoff, resource isolation, and task disposal.
import assert from "node:assert/strict";
import { setImmediate as yieldToTasks } from "node:timers/promises";
import { afterEach, describe, it } from "node:test";
import { createRoot } from "solid-js";
import { vi } from "@tests/expect";
import type { HistoryEventsHandlers } from "@sdk/api.gen";
import type { EventStats, HistoryEvent } from "@sdk/types.gen";
import { api } from "./api";
import type { TaskHistory as LiveHistory } from "./taskEventTimeline";
import { createTaskEventTimeline } from "./taskEventTimeline";

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
    return Response.json(events.filter((e) => e.recordID >= first && e.recordID <= last));
  });
  return connections;
}

function text(recordID: number, index: number, value: string): HistoryEvent {
  return { recordID, index, message: { kind: "text", ts: recordID, text: { text: value } } };
}

function input(recordID: number, value: string): HistoryEvent {
  return { recordID, index: 0, message: { kind: "userInput", ts: recordID, userInput: { text: value } } };
}

async function loaded(owner: LiveHistory) {
  for (let attempt = 0; owner.loading() && attempt < 20; attempt++) await yieldToTasks();
  assert.equal(owner.loading(), false);
}

function values(owner: LiveHistory) {
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

describe("createLiveTaskHistory", () => {
  it("automatically loads the complete backlog without navigation, including outputless chunks", async () => {
    const connections = capture([text(2, 0, "old"), text(520, 0, "recent")], 520);
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
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
      release.resolve(Response.json([events[0]]));
      await loaded(owner);
      assert.deepEqual(values(owner), ["old", "recent", "during backfill"]);
      assert.ok(owner.epoch() > before);
      connections[0]?.handlers.onMessage(input(522, "after backfill"));
      assert.deepEqual(values(owner), ["old", "recent", "during backfill", "after backfill"]);
    } finally {
      owner.dispose();
    }
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
    }));
    try {
      await requested.promise;
      for (let index = 0; index < 2600; index++) connections[0]?.handlers.onMessage(text(2, index, `pending ${index}`));
      connections[0]?.handlers.onMessage(input(3, "after pending"));
      assert.deepEqual(owner.messages(), []);
      release.resolve(Response.json([text(1, 0, "snapshot")]));
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
    }));
    try {
      await requested.promise;
      assert.deepEqual(owner.messages(), []);
      assert.equal(owner.preview()?.response, "x".repeat(16_000));
      release.resolve(Response.json([old]));
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
    }));
    try {
      await requested.promise;
      replacement = true;
      events.splice(0, events.length, text(2, 0, "replacement prefix"), text(520, 0, "replacement tail"));
      connections[0]?.handlers.onReset?.({ bootstrapURL: "/bootstrap" });
      await loaded(owner);
      assert.equal(connections[0]?.close.mock.calls.length, 1);
      assert.equal(connections.length, 2);
      release.resolve(Response.json([text(2, 0, "obsolete response")]));
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
    }));
    await requested.promise;
    assert.deepEqual(owner.messages(), []);
    assert.equal(owner.preview()?.response, "recent");
    owner.dispose();
    assert.equal(connections[0]?.close.mock.calls.length, 1);
    release.resolve(Response.json([text(2, 0, "obsolete")]));
    connections[0]?.handlers.onMessage(input(521, "obsolete stream"));
    await yieldToTasks();
    assert.deepEqual(owner.messages(), []);
  });

  it("merges independent resource samples by timestamp and bounds only the sample ring", async () => {
    const connections = capture([text(2, 0, "recent")], 2);
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
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

  it("joins prices by exact raw result position and aborts them when the owner changes", async () => {
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
    capture(
      [
        { recordID: 2, index: 0, message: { kind: "result", ts: 2, result: child, source: { agentID: "child" } } },
        { recordID: 2, index: 1, message: { kind: "result", ts: 2, result: root } },
      ],
      2,
    );
    const original = fetchMock.getMockImplementation();
    assert.ok(original);
    const delayed = Promise.withResolvers<Response>();
    let priceSignal: AbortSignal | null = null;
    let pending = false;
    fetchMock.mockImplementation((url, init) => {
      if (!String(url).includes("/costs?")) return original(url, init);
      priceSignal = init?.signal ?? null;
      return pending
        ? delayed.promise
        : Promise.resolve(
            Response.json([
              { recordID: 2, index: 0, model: "child", cost: { usd: 99, source: "reported" }, totalUSD: 99 },
              { recordID: 2, index: 1, model: "rerouted", cost: { usd: 0, source: "reported" }, totalUSD: 0 },
            ]),
          );
    });
    const owner = createRoot((dispose) => ({
      dispose,
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
    }));
    try {
      await loaded(owner);
      const costs = await owner.readCosts(new AbortController().signal);
      assert.equal(costs.length, 1);
      assert.equal(costs[0]?.result, owner.messages()[1]?.result);
      assert.equal(costs[0]?.model, "rerouted");
      assert.equal(costs[0]?.cost?.usd, 0);
      pending = true;
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
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
      ...createTaskEventTimeline({ taskId: () => "task", taskState: () => "running" }),
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
