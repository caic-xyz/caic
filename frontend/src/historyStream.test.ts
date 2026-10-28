// Tests incremental history-array decoding, early delivery, oversized events, and cancellation.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { readHistoryArray } from "./historyStream";

describe("readHistoryArray", () => {
  it("delivers an event before the response finishes and preserves split strings", async () => {
    const first = { recordID: 2, index: 0, message: { kind: "text", ts: 1, text: { text: 'escaped \\" [ ] { } Ω' } } };
    const second = { recordID: 3, index: 0, message: { kind: "text", ts: 2, text: { text: "second" } } };
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        controller = c;
      },
    });
    const encoder = new TextEncoder();
    const reader = readHistoryArray(body, new AbortController().signal);
    const encoded = encoder.encode("[" + JSON.stringify(first) + ",");
    for (let i = 0; i < encoded.length; i++) controller.enqueue(encoded.slice(i, i + 1));
    assert.equal((await reader.next()).value?.message.text?.text, first.message.text.text);
    controller.enqueue(encoder.encode(JSON.stringify(second) + "]"));
    controller.close();
    assert.equal((await reader.next()).value?.message.text?.text, second.message.text.text);
    assert.equal((await reader.next()).done, true);
  });

  it("does not reject or truncate a single oversized event", async () => {
    const event = { recordID: 1, index: 0, message: { kind: "text", ts: 1, text: { text: "x".repeat(3 << 20) } } };
    const values = [];
    const body = new Response(JSON.stringify([event])).body;
    assert.ok(body);
    for await (const entry of readHistoryArray(body, new AbortController().signal)) values.push(entry);
    assert.equal(values.length, 1);
    assert.equal(values[0]?.message.text?.text.length, 3 << 20);
  });

  it("cancels the response when its consumer stops", async () => {
    let cancelled = false;
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(
          new TextEncoder().encode(
            '[{"recordID":1,"index":0,"message":{"kind":"text","ts":1,"text":{"text":"ready"}}},',
          ),
        );
      },
      cancel() {
        cancelled = true;
      },
    });
    for await (const entry of readHistoryArray(body, new AbortController().signal)) {
      assert.equal(entry.recordID, 1);
      break;
    }
    assert.equal(cancelled, true);
  });

  for (const text of ["[", '[{"recordID":1}', "[{}]", "[{},]", "[]garbage"]) {
    it(`reports malformed or invalid history: ${text}`, async () => {
      await assert.rejects(async () => {
        const body = new Response(text).body;
        assert.ok(body);
        for await (const entry of readHistoryArray(body, new AbortController().signal)) void entry;
      });
    });
  }
});
