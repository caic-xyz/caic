// Tests bounded line-framed history decoding, split UTF-8, early delivery, validation, and reader cancellation.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { readHistoryBatches } from "./historyStream";

const first = {
  recordID: 2,
  index: 0,
  message: { kind: "text", ts: 1, text: { text: 'escaped \\" [ ] { }\nΩ' } },
};
const second = { recordID: 2, index: 1, message: { kind: "text", ts: 2, text: { text: "second" } } };

function body(text: string): ReadableStream<Uint8Array> {
  const stream = new Response(text).body;
  assert.ok(stream);
  return stream;
}

describe("readHistoryBatches", () => {
  it("delivers a batch before EOF while UTF-8 and strings cross every byte boundary", async () => {
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    const stream = new ReadableStream<Uint8Array>({
      start(c) {
        controller = c;
      },
    });
    const reader = readHistoryBatches(stream, new AbortController().signal);
    const encoded = new TextEncoder().encode("[\n" + JSON.stringify(first) + "\n");
    for (let i = 0; i < encoded.length; i++) controller.enqueue(encoded.slice(i, i + 1));
    const a = (await reader.next()).value;
    assert.equal(a?.length, 1);
    assert.equal(a?.[0]?.recordID, 2);
    assert.equal(a?.[0]?.index, 0);
    assert.equal(a?.[0]?.message.text?.text, first.message.text.text);
    controller.enqueue(new TextEncoder().encode("," + JSON.stringify(second) + "\n]\n"));
    controller.close();
    const b = (await reader.next()).value;
    assert.equal(b?.length, 1);
    assert.equal(b?.[0]?.recordID, 2);
    assert.equal(b?.[0]?.index, 1);
    assert.equal(b?.[0]?.message.text?.text, "second");
    assert.equal((await reader.next()).done, true);
  });

  it("keeps batches bounded when one raw record emits thousands of outputs", async () => {
    const events = Array.from({ length: 2600 }, (_, index) => ({ ...first, index }));
    const stream = body("[\n" + events.map((event) => JSON.stringify(event)).join("\n,") + "\n]\n");
    const received = [];
    let batches = 0;
    for await (const batch of readHistoryBatches(stream, new AbortController().signal)) {
      assert.ok(batch.length > 1 && batch.length < events.length);
      batches++;
      received.push(...batch);
    }
    assert.ok(batches > 1);
    assert.deepEqual(
      received.map((event) => [event.recordID, event.index, event.message.text?.text]),
      events.map((event) => [event.recordID, event.index, event.message.text.text]),
    );
  });

  it("preserves an oversized event split across many response reads", async () => {
    const event = { ...first, message: { ...first.message, text: { text: "x".repeat(3 << 20) } } };
    const encoded = new TextEncoder().encode("[\n" + JSON.stringify(event) + "\n]\n");
    let offset = 0;
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (offset === encoded.length) controller.close();
        else {
          controller.enqueue(encoded.subarray(offset, offset + 4096));
          offset = Math.min(offset + 4096, encoded.length);
        }
      },
    });
    const values = [];
    for await (const batch of readHistoryBatches(stream, new AbortController().signal)) values.push(...batch);
    assert.equal(values.length, 1);
    assert.equal(values[0]?.message.text?.text, event.message.text.text);
  });

  it("cancels the response and releases its lock when the consumer stops", async () => {
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode("[\n" + JSON.stringify(first) + "\n"));
      },
      cancel() {
        cancelled = true;
      },
    });
    for await (const batch of readHistoryBatches(stream, new AbortController().signal)) {
      assert.equal(batch.length, 1);
      assert.equal(batch[0]?.message.text?.text, first.message.text.text);
      break;
    }
    assert.equal(cancelled, true);
    assert.equal(stream.locked, false);
  });

  it("aborts a stalled body reader without publishing another batch", async () => {
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      cancel() {
        cancelled = true;
      },
    });
    const controller = new AbortController();
    const reader = readHistoryBatches(stream, controller.signal);
    const next = reader.next();
    controller.abort();
    await assert.rejects(next, { name: "AbortError" });
    assert.equal(cancelled, true);
    assert.equal(stream.locked, false);
  });

  it("rejects invalid UTF-8 rather than replacing conversation bytes", async () => {
    const stream = new Response(new Uint8Array([91, 10, 255, 10, 93, 10])).body;
    assert.ok(stream);
    await assert.rejects(async () => {
      for await (const batch of readHistoryBatches(stream, new AbortController().signal)) void batch;
    });
    assert.equal(stream.locked, false);
  });

  it("accepts outputless ranges and a closing bracket at EOF", async () => {
    for (const text of ["[\n]\n", "[\n]"]) {
      const batches = [];
      for await (const batch of readHistoryBatches(body(text), new AbortController().signal)) batches.push(batch);
      assert.deepEqual(batches, []);
    }
  });

  for (const text of [
    "",
    "[\n",
    '[\n{"recordID":1}\n]\n',
    "[\n{}\n]\n",
    "[\n,{}\n]\n",
    "[\n]\n{}\n",
    "[\n" + JSON.stringify(first) + "\n" + JSON.stringify(second) + "\n]\n",
    "[\n" + JSON.stringify(first) + ",\n]\n",
    "[\n" + JSON.stringify(first) + "\n,]\n",
    "[\n" + JSON.stringify(first) + "\n",
    '[\n{"recordID":1,"index":0,"message":{"kind":"text","ts":0}}\n]\n',
  ])
    it(`rejects malformed, truncated, or invalid history: ${text}`, async () => {
      const stream = body(text);
      await assert.rejects(async () => {
        for await (const batch of readHistoryBatches(stream, new AbortController().signal)) void batch;
      });
      assert.equal(stream.locked, false);
    });
});
