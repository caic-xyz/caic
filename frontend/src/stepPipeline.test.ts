// Tests early STEP indexing, bounded pulls, cancellation, and atomic validation before geometry import.
import { afterEach, describe, it } from "node:test";
import { gzipSync, zipSync } from "three/addons/libs/fflate.module.js";
import { expect, vi } from "@tests/expect";
import { createFacetedStep } from "../tests/step-fixture";
import { readStepInput } from "./stepPipeline";
import { importStep } from "./stepImport";
import type { CadLoadProgress, StepWorkerTimings } from "./stepProtocol";

const fetchMock = vi.spyOn(globalThis, "fetch");
afterEach(() => fetchMock.mockReset());
function timings(): StepWorkerTimings {
  return {
    startedAt: 0,
    sentAt: 0,
    downloadMs: 0,
    decompressMs: 0,
    directParseMs: 0,
    packingMs: 0,
    occtInitMs: 0,
    occtParseMs: 0,
  };
}

describe("streamed STEP input", () => {
  for (const kind of ["plain", "zip", "gzip"] as const) {
    it(`indexes ${kind} records while the network is gated and publishes geometry only after validation`, async () => {
      const model = createFacetedStep(8, false);
      const bytes = kind === "plain" ? model : kind === "zip" ? zipSync({ "part.step": model }) : gzipSync(model);
      const path = kind === "plain" ? "part.step" : kind === "zip" ? "part.stpz" : "part.step.gz";
      const gate = Promise.withResolvers<void>();
      const indexed = Promise.withResolvers<CadLoadProgress>();
      const updates: CadLoadProgress[] = [];
      let pulls = 0;
      let cancelled = false;
      const prefix = bytes.length - (kind === "plain" ? 64 : 8);
      fetchMock.mockResolvedValue(
        new Response(
          new ReadableStream<Uint8Array>(
            {
              async pull(controller) {
                if (!pulls++) controller.enqueue(bytes.subarray(0, prefix));
                else {
                  await gate.promise;
                  controller.enqueue(bytes.subarray(prefix));
                  controller.close();
                }
              },
              cancel() {
                cancelled = true;
              },
            },
            { highWaterMark: 0 },
          ),
          { headers: { "Content-Length": String(bytes.length) } },
        ),
      );
      let settled = false;
      const work = timings();
      const pending = readStepInput(
        "task",
        path,
        new AbortController().signal,
        (value) => {
          updates.push(value);
          if ((value.indexedRecords ?? 0) > 100) indexed.resolve(value);
        },
        work,
      ).then((reader) => {
        settled = true;
        return importStep(reader, work);
      });
      const early = await indexed.promise;
      expect(early.completed).toBe(prefix);
      expect(early.decodedBytes).toBeGreaterThan(1000);
      expect(settled).toBe(false);
      expect(pulls).toBeLessThanOrEqual(2);
      gate.resolve();
      expect(await pending).toHaveLength(8);
      expect(cancelled).toBe(false);
      expect(updates.every((value) => value.stage === "download")).toBe(true);
      expect(work.downloadMs).toBeGreaterThanOrEqual(0);
      expect(work.decompressMs).toBeGreaterThanOrEqual(0);
      expect(work.directParseMs).toBeGreaterThan(0);
      expect(work.occtInitMs).toBe(0);
    });
  }

  it("reports growing decoded and indexed counts inside one fully received browser chunk", async () => {
    const model = createFacetedStep(256, false);
    const updates: CadLoadProgress[] = [];
    fetchMock.mockResolvedValue(new Response(model, { headers: { "Content-Length": String(model.length) } }));
    const reader = await readStepInput(
      "task",
      "part.step",
      new AbortController().signal,
      (value) => updates.push(value),
      timings(),
    );
    const decoded = updates.filter((value) => (value.decodedBytes ?? 0) > 0);
    expect(decoded.length).toBeGreaterThan(1);
    expect(decoded[0].decodedBytes).toBeLessThan(model.length);
    expect(decoded.at(-1)?.decodedBytes).toBe(model.length);
    expect(decoded.every((value) => value.completed === model.length && value.total === model.length)).toBe(true);
    for (let i = 1; i < decoded.length; i++) {
      expect(decoded[i].decodedBytes).toBeGreaterThan(decoded[i - 1].decodedBytes ?? 0);
      expect(decoded[i].indexedRecords).toBeGreaterThanOrEqual(decoded[i - 1].indexedRecords ?? 0);
    }
    expect(await importStep(reader)).toHaveLength(256);
  });

  it("cancels the network when the indexed prefix consumer aborts", async () => {
    let cancelled = false;
    let pulls = 0;
    const model = createFacetedStep(4, false);
    const abort = new AbortController();
    fetchMock.mockResolvedValue(
      new Response(
        new ReadableStream<Uint8Array>(
          {
            pull(controller) {
              pulls++;
              controller.enqueue(model);
            },
            cancel() {
              cancelled = true;
            },
          },
          { highWaterMark: 0 },
        ),
      ),
    );
    await expect(
      readStepInput(
        "task",
        "part.step",
        abort.signal,
        (value) => {
          if ((value.indexedRecords ?? 0) > 0) abort.abort();
        },
        timings(),
      ),
    ).rejects.toThrow();
    expect(cancelled).toBe(true);
    expect(pulls).toBe(1);
  });

  it("rejects a late corrupt archive despite having indexed the complete model", async () => {
    const model = createFacetedStep(4, false);
    const archive = zipSync({ "part.step": model });
    const view = new DataView(archive.buffer);
    let central = -1;
    for (let i = 0; i < archive.length - 4; i++)
      if (view.getUint32(i, true) === 0x02014b50) {
        central = i;
        break;
      }
    if (central < 0) throw new Error("ZIP fixture has no central directory");
    archive[central + 16] ^= 1;
    const indexed = Promise.withResolvers<void>();
    const gate = Promise.withResolvers<void>();
    let pulls = 0;
    fetchMock.mockResolvedValue(
      new Response(
        new ReadableStream<Uint8Array>(
          {
            async pull(controller) {
              if (!pulls++) controller.enqueue(archive.subarray(0, central));
              else {
                await gate.promise;
                controller.enqueue(archive.subarray(central));
                controller.close();
              }
            },
          },
          { highWaterMark: 0 },
        ),
      ),
    );
    const pending = readStepInput(
      "task",
      "part.stpz",
      new AbortController().signal,
      (value) => {
        if ((value.indexedRecords ?? 0) > 100) indexed.resolve();
      },
      timings(),
    );
    await indexed.promise;
    gate.resolve();
    await expect(pending).rejects.toThrow();
  });
});
