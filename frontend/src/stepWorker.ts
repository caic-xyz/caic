// Owns STEP downloads, decompression, OCCT parsing, and transfer of mesh buffers from an expendable worker.

import { readTaskBinaryFile } from "./taskFile";
import { decompressStep } from "./stepCompression";
import { isCompressedStepPath } from "./runtimePath";
import { importStep } from "./stepImport";
import type { CadLoadProgress, CadProgress, StepMessage, StepRequest } from "./stepProtocol";

function send(message: StepMessage, transfers: ArrayBuffer[]) {
  globalThis.postMessage(message, { transfer: transfers });
}

// This frame owns the download only until decompression finishes. The parse
// chain then owns the decoded bytes, with no retained outer input-buffer copy.
async function readStepInput(request: StepRequest, progress: CadProgress) {
  const { taskId, path } = request;
  const buffer = await readTaskBinaryFile(taskId, path, new AbortController().signal, (completed, total) => {
    progress({ stage: "download", completed, total });
  });
  if (isCompressedStepPath(path)) progress({ stage: "decompress", completed: 0, total: null });
  return decompressStep(buffer, path, (completed, total) => {
    progress({ stage: "decompress", completed, total });
  }).then((decoded) => {
    progress({ stage: "parse", completed: null, total: null });
    return decoded;
  });
}

globalThis.onmessage = async (event: MessageEvent<StepRequest>) => {
  let previousStage: CadLoadProgress["stage"] | null = null;
  let previousTime = 0;
  function progress(value: CadLoadProgress) {
    const now = performance.now();
    if (value.stage !== previousStage || now - previousTime >= 50 || value.completed === value.total) {
      send({ type: "progress", progress: value }, []);
      previousStage = value.stage;
      previousTime = now;
    }
  }
  try {
    const meshes = await readStepInput(event.data, progress).then(importStep);
    const transfers = new Set<ArrayBuffer>();
    for (const mesh of meshes) {
      transfers.add(mesh.positions.buffer);
      transfers.add(mesh.normals.buffer);
      if (mesh.indices) transfers.add(mesh.indices.buffer);
    }
    send({ type: "result", meshes }, [...transfers]);
  } catch (error) {
    send({ type: "error", message: error instanceof Error ? error.message : "Failed to import STEP model" }, []);
  }
};
