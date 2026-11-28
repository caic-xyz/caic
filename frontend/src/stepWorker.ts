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
  const progressInterval = 100;
  let previousStage = "";
  let previousTime = 0;
  let pending: CadLoadProgress | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;
  function sendProgress(value: CadLoadProgress) {
    clearTimeout(timer);
    timer = undefined;
    pending = null;
    previousTime = performance.now();
    send({ type: "progress", progress: value }, []);
  }
  function progress(value: CadLoadProgress) {
    // Deliver the first received bytes and first indexed prefix even when
    // the next network read stalls within the normal throttle interval.
    const inputState =
      value.stage !== "download"
        ? ""
        : value.indexedRecords === null
          ? "decoded"
          : (value.indexedRecords ?? 0) > 0
            ? "indexed"
            : (value.completed ?? 0) > 0
              ? "receiving"
              : "waiting";
    const stage = `${value.stage}:${value.parseStep ?? ""}:${inputState}`;
    // Receiving all network bytes does not finish indexing buffered slices.
    // Those updates retain the normal cadence even at 100% received.
    const finished = value.stage !== "download" && value.total !== null && value.completed === value.total;
    const remaining = progressInterval - (performance.now() - previousTime);
    if (stage !== previousStage || remaining <= 0 || finished) {
      previousStage = stage;
      sendProgress(value);
    } else if (value.stage === "download") {
      // A network pause may leave this as the last update until EOF.
      pending = value;
      timer ??= setTimeout(() => {
        if (pending) sendProgress(pending);
      }, remaining);
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
  } finally {
    clearTimeout(timer);
  }
};
