// Tests current-only paging, atomic live context changes, exact handoff, and disclosure cancellation.
import { defined } from "@tests/history";
import assert from "node:assert/strict";
import { afterEach, describe, it } from "node:test";
import { setImmediate as yieldToTasks } from "node:timers/promises";
import { createRoot, createEffect } from "solid-js";
import { vi } from "@tests/expect";
import { historyResponse, historyManifest, fixtureHistoryResponse } from "@tests/history";
import type { HistoryEventsHandlers } from "@sdk/api.gen";
import type { HistoryEvent, HistoryManifest } from "@sdk/types.gen";
import { api } from "./api";
import { createTaskEventTimeline, type TaskHistory } from "./taskEventTimeline";
const fetchMock = vi.spyOn(globalThis, "fetch");
const streamMock = vi.spyOn(api, "historyEvents");
afterEach(() => {
  fetchMock.mockReset();
  streamMock.mockReset();
  vi.useRealTimers();
});
const text = (recordID: number, value: string, index: number): HistoryEvent => ({
  recordID,
  index,
  message: { kind: "text", ts: recordID, text: { text: value } },
});
function fixture(events: HistoryEvent[], value: HistoryManifest) {
  const connections: {
    handlers: HistoryEventsHandlers;
    close: ReturnType<typeof vi.fn>;
  }[] = [];
  const requests: string[] = [];
  streamMock.mockImplementation((_id, _cursor, handlers) => {
    const close = vi.fn();
    connections.push({ handlers, close });
    return { close } as unknown as EventSource;
  });
  fetchMock.mockImplementation(async (input, init) => {
    init?.signal?.throwIfAborted();
    const url = String(input);
    requests.push(url);
    return url.endsWith("/manifest") ? Response.json(value) : fixtureHistoryResponse(url, events, "");
  });
  const history = createRoot((dispose) => ({ ...createTaskEventTimeline({ taskId: () => "task" }), dispose }));
  return { ...history, connections, requests };
}
async function loaded(owner: TaskHistory) {
  for (let i = 0; owner.loading() && i < 30; i++) await yieldToTasks();
  assert.equal(owner.error(), null);
  assert.equal(owner.loading(), false);
}
const values = (owner: TaskHistory) => owner.messages().flatMap((message) => (message.text ? [message.text.text] : []));
describe("section history owner", () => {
  it("requests only current context pages, retaining summaries without archived payloads", async () => {
    const events = [text(1, "archived", 0), text(513, "current", 0), text(520, "latest", 0)];
    const manifest = historyManifest(events, "");
    manifest.sections = [
      {
        id: "1",
        body: { first: defined(events[0]), last: defined(events[0]) },
        turnCount: 1,
        textCount: 1,
        toolCount: 0,
        durationMs: 1,
        skills: [],
      },
      {
        id: "2",
        body: { first: defined(events[1]), last: defined(events[2]) },
        turnCount: 1,
        textCount: 2,
        toolCount: 0,
        durationMs: 1,
        skills: [],
      },
    ];
    const owner = fixture(events, manifest);
    try {
      await loaded(owner);
      assert.deepEqual(values(owner), ["current", "latest"]);
      assert.deepEqual(
        owner.requests.filter((url) => url.includes("/slice/")),
        ["/history/owner/3/test/slice/513/0/520/0"],
      );
      assert.equal(owner.manifest()?.sections.length, 2);
      assert.equal(owner.connections.length, 1);
      const archived = await owner.readSection(defined(manifest.sections[0]), new AbortController().signal);
      assert.equal(archived[0]?.message.text?.text, "archived");
      assert.deepEqual(values(owner), ["current", "latest"]);
    } finally {
      owner.dispose();
    }
    assert.equal(owner.messages().length, 0);
    assert.equal(owner.manifest(), null);
  });
  it("uses exact normalized edges around stable raw pages without neighboring outputs", async () => {
    const events = [
      text(256, "excluded", 0),
      text(256, "edge", 1),
      text(257, "last", 0),
      text(257, "excluded after", 1),
    ];
    const manifest = historyManifest(events, "");
    defined(manifest.sections[0]).body = { first: defined(events[1]), last: defined(events[2]) };
    const owner = fixture(events, manifest);
    try {
      await loaded(owner);
      assert.deepEqual(values(owner), ["edge", "last"]);
      assert.deepEqual(
        owner.requests.filter((url) => url.includes("/slice/")),
        ["/history/owner/3/test/slice/256/1/256/end", "/history/owner/3/test/slice/257/0/257/0"],
      );
    } finally {
      owner.dispose();
    }
  });
  it("cancels obsolete current pages when live compaction arrives during initial loading", async () => {
    const events = [text(1, "old", 0)];
    const manifest = historyManifest(events, "");
    let release: (response: Response) => void = () => {};
    const blocked = new Promise<Response>((resolve) => {
      release = resolve;
    });
    const owner = fixture(events, manifest);
    const original = defined(fetchMock.getMockImplementation());
    let obsolete: AbortSignal | null = null;
    fetchMock.mockImplementation((input, init) => {
      if (String(input).endsWith("/slice/1/0/1/0")) {
        obsolete = init?.signal ?? null;
        return blocked;
      }
      return original(input, init);
    });
    try {
      for (let i = 0; !obsolete && i < 20; i++) await yieldToTasks();
      assert.ok(obsolete);
      const boundary: HistoryEvent = {
        recordID: 2,
        index: 0,
        message: { kind: "system", ts: 2, system: { subtype: "compact_boundary" } },
      };
      const current = text(3, "new context", 0);
      events.push(boundary, current);
      const next = structuredClone(manifest);
      next.bootstrap.through = 3;
      next.sections.push({
        id: "2",
        body: { first: boundary, last: current },
        boundary,
        turnCount: 1,
        textCount: 1,
        toolCount: 0,
        durationMs: 1,
        skills: [],
      });
      const handlers = defined(owner.connections[0]).handlers;
      handlers.onMessage(boundary);
      handlers.onMessage(current);
      handlers.onManifest?.(next);
      await loaded(owner);
      assert.equal((obsolete as AbortSignal).aborted, true);
      assert.deepEqual(values(owner), ["new context"]);
      release(historyResponse([defined(events[0])]));
      await yieldToTasks();
      assert.deepEqual(values(owner), ["new context"]);
      assert.equal(owner.connections.length, 1);
    } finally {
      owner.dispose();
    }
  });
  it("publishes append-only live output once and advances checkpoints with no outputs", async () => {
    vi.useFakeTimers();
    const events = [text(1, "first", 0)];
    const owner = fixture(events, historyManifest(events, ""));
    const lengths: number[] = [];
    const watch = createRoot((dispose) => {
      createEffect(() => lengths.push(owner.messages().length));
      return dispose;
    });
    try {
      await loaded(owner);
      const initial = owner.messages();
      const handlers = defined(owner.connections[0]).handlers;
      handlers.onMessage(text(2, "second", 0));
      handlers.onMessage(text(2, "second", 0));
      handlers.onMessage(text(2, "third", 1));
      vi.advanceTimersByTime(100);
      assert.equal(owner.messages(), initial);
      assert.deepEqual(values(owner), ["first", "second", "third"]);
      assert.equal(lengths.at(-1), 3);
      handlers.onCheckpoint?.({ cursor: "3.test.9.end", through: 9 });
      assert.equal(owner.through(), 9);
    } finally {
      watch();
      owner.dispose();
    }
  });
  it("aborts pending disclosure reads and rejects retired publications on disposal", async () => {
    const events = [text(1, "current", 0)];
    const manifest = historyManifest(events, "");
    const owner = fixture(events, manifest);
    await loaded(owner);
    let signal: AbortSignal | null = null;
    let release: (value: Response) => void = () => {};
    fetchMock.mockImplementation((_input, init) => {
      signal = init?.signal ?? null;
      return new Promise((resolve) => {
        release = resolve;
      });
    });
    const read = owner.readSection(defined(manifest.sections[0]), new AbortController().signal);
    owner.dispose();
    assert.equal((signal as AbortSignal | null)?.aborted, true);
    release(historyResponse(events));
    await assert.rejects(read, { name: "AbortError" });
    defined(owner.connections[0]).handlers.onMessage(text(2, "retired", 0));
    assert.deepEqual(values(owner), []);
  });
});
it("waits for a split root-boundary manifest despite timers, checkpoints, and immediate asks", async () => {
  vi.useFakeTimers();
  const events = [text(1, "old current", 0)];
  const first = historyManifest(events, "");
  const owner = fixture(events, first);
  try {
    await loaded(owner);
    const initial = owner.messages();
    const boundary: HistoryEvent = {
      recordID: 2,
      index: 0,
      message: { kind: "system", ts: 2, system: { subtype: "compact_boundary" } },
    };
    const input: HistoryEvent = {
      recordID: 3,
      index: 0,
      message: { kind: "userInput", ts: 3, userInput: { text: "next input" } },
    };
    const ask: HistoryEvent = {
      recordID: 4,
      index: 0,
      message: { kind: "ask", ts: 4, ask: { toolUseID: "ask", questions: [] } },
    };
    const stream = defined(owner.connections[0]).handlers;
    for (const event of [boundary, input, ask]) stream.onMessage(event);
    stream.onCheckpoint?.({ cursor: "3.test.4.end", through: 4 });
    vi.advanceTimersByTime(5000);
    assert.equal(owner.messages(), initial);
    assert.deepEqual(values(owner), ["old current"]);
    const next = structuredClone(first);
    next.bootstrap.through = 4;
    next.sections.push({
      id: "2",
      body: { first: boundary, last: ask },
      boundary,
      turnCount: 1,
      textCount: 0,
      toolCount: 0,
      durationMs: 2,
      skills: [],
    });
    stream.onManifest?.(next);
    assert.equal(
      owner.messages().some((event) => event.text?.text === "old current"),
      false,
    );
    assert.equal(owner.messages().at(-1)?.kind, "ask");
  } finally {
    owner.dispose();
  }
});
it("does not hold ordinary live output behind an unannounced child boundary", async () => {
  vi.useFakeTimers();
  const events = [text(1, "root", 0)];
  const owner = fixture(events, historyManifest(events, ""));
  try {
    await loaded(owner);
    const stream = defined(owner.connections[0]).handlers;
    stream.onMessage({
      recordID: 2,
      index: 0,
      message: { kind: "system", ts: 2, source: { agentID: "child" }, system: { subtype: "compact_boundary" } },
    });
    stream.onMessage(text(3, "continued root", 0));
    vi.advanceTimersByTime(100);
    assert.deepEqual(values(owner), ["root", "continued root"]);
  } finally {
    owner.dispose();
  }
});
it("bounds current page concurrency and publishes pages in raw order despite reversed completion", async () => {
  const events = Array.from({ length: 6 }, (_, index) => text(index * 256 + 1, String(index), 0));
  const value = historyManifest(events, "");
  const owner = fixture(events, value);
  const original = defined(fetchMock.getMockImplementation());
  const waiting: {
    release: (response: Response) => void;
    event: HistoryEvent;
  }[] = [];
  fetchMock.mockImplementation((input, init) => {
    const first = String(input).match(/\/slice\/(\d+)\//)?.[1];
    if (!first) return original(input, init);
    const event = defined(events.find((candidate) => candidate.recordID === Number(first)));
    return new Promise<Response>((release) => {
      waiting.push({ release, event });
    });
  });
  try {
    for (let i = 0; waiting.length < 4 && i < 20; i++) await yieldToTasks();
    assert.equal(waiting.length, 4);
    assert.equal(owner.messages().length, 0);
    for (const item of waiting.slice(0, 4).reverse()) item.release(historyResponse([item.event]));
    for (let i = 0; waiting.length < 6 && i < 20; i++) await yieldToTasks();
    assert.equal(waiting.length, 6);
    for (const item of waiting.slice(4).reverse()) item.release(historyResponse([item.event]));
    await loaded(owner);
    assert.deepEqual(values(owner), ["0", "1", "2", "3", "4", "5"]);
  } finally {
    owner.dispose();
  }
});

it("bounds setup disclosure by its last log or initial init instead of task history", async () => {
  const events: HistoryEvent[] = [
    { recordID: 2, index: 0, message: { kind: "log", ts: 2, log: { line: "setup" } } },
    {
      recordID: 4,
      index: 0,
      message: {
        kind: "init",
        ts: 4,
        init: { sessionID: "root", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
      },
    },
    text(700, "current", 0),
  ];
  const value = historyManifest(events, "");
  const owner = fixture(events, value);
  try {
    await loaded(owner);
    await owner.readSetup(new AbortController().signal);
    assert.deepEqual(
      owner.requests.filter((url) => url.includes("/setup/")),
      ["/history/owner/3/test/setup/4"],
    );
  } finally {
    owner.dispose();
  }
});

it("returns an empty setup disclosure before any setup or init without a request", async () => {
  const events = [text(1, "current", 0)];
  const owner = fixture(events, historyManifest(events, ""));
  try {
    await loaded(owner);
    assert.deepEqual(await owner.readSetup(new AbortController().signal), []);
    assert.deepEqual(
      owner.requests.filter((url) => url.includes("/setup/")),
      [],
    );
  } finally {
    owner.dispose();
  }
});
