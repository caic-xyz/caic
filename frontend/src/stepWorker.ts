// Owns STEP downloads, decompression, throttled parsing progress, and transfer of mesh buffers from an expendable worker.

import { importStep } from "./stepImport";
import { readStepInput } from "./stepPipeline";
import { timingNow } from "./stepTimings";
import type { CadLoadProgress, StepMessage, StepRequest, StepWorkerTimings } from "./stepProtocol";

function send(message: StepMessage, transfers: ArrayBuffer[]) {
  globalThis.postMessage(message, { transfer: transfers });
}

globalThis.onmessage = async (event: MessageEvent<StepRequest>) => {
  const timings: StepWorkerTimings = {
    startedAt: timingNow(),
    sentAt: 0,
    downloadMs: 0,
    decompressMs: 0,
    directParseMs: 0,
    occtInitMs: 0,
    occtParseMs: 0,
    packingMs: 0,
  };
  let previousStage = "";
  let previousTime = 0;
  function progress(value: CadLoadProgress) {
    const now = performance.now();
    // Deliver the first indexed prefix even when it arrives within the normal
    // throttle interval and the next network read then waits indefinitely.
    const inputState =
      value.stage !== "download"
        ? ""
        : value.indexedRecords === null
          ? "decoded"
          : (value.indexedRecords ?? 0) > 0
            ? "indexed"
            : "waiting";
    const stage = `${value.stage}:${value.parseStep ?? ""}:${inputState}`;
    // Receiving all network bytes does not finish indexing buffered slices.
    // Those updates retain the normal cadence even at 100% received.
    const finished = value.stage !== "download" && value.total !== null && value.completed === value.total;
    if (stage !== previousStage || now - previousTime >= 100 || finished) {
      send({ type: "progress", progress: value }, []);
      previousStage = stage;
      previousTime = now;
    }
  }
  try {
    const meshes = await readStepInput(
      event.data.taskId,
      event.data.path,
      new AbortController().signal,
      progress,
      timings,
    ).then((reader) => importStep(reader, timings, progress));
    const transfers = new Set<ArrayBuffer>();
    for (const mesh of meshes) {
      transfers.add(mesh.positions.buffer);
      transfers.add(mesh.normals.buffer);
      if (mesh.indices) transfers.add(mesh.indices.buffer);
    }
    timings.sentAt = timingNow();
    send({ type: "result", meshes, timings }, [...transfers]);
  } catch (error) {
    send({ type: "error", message: error instanceof Error ? error.message : "Failed to import STEP model" }, []);
  }
};
