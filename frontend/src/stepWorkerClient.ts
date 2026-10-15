// Runs one cancellable STEP worker per load and releases its WASM heap after completion or cancellation.

import type { CadProgress, StepMesh, StepMessage, StepRequest } from "./stepProtocol";

export function loadStep(
  taskId: string,
  path: string,
  signal: AbortSignal,
  progress: CadProgress,
): Promise<StepMesh[]> {
  return new Promise((resolve, reject) => {
    signal.throwIfAborted();
    const worker = new Worker(new URL("./stepWorker.ts", import.meta.url), { type: "module" });
    function cleanup() {
      signal.removeEventListener("abort", abort);
      worker.terminate();
    }
    function abort() {
      cleanup();
      reject(signal.reason);
    }
    signal.addEventListener("abort", abort, { once: true });
    worker.onmessage = (event: MessageEvent<StepMessage>) => {
      const message = event.data;
      switch (message.type) {
        case "progress":
          progress(message.progress);
          break;
        case "result":
          cleanup();
          resolve(message.meshes);
          break;
        case "error":
          cleanup();
          reject(new Error(message.message));
          break;
      }
    };
    worker.onerror = (event) => {
      event.preventDefault();
      cleanup();
      reject(new Error(event.message || "Failed to start STEP import worker"));
    };
    worker.onmessageerror = () => {
      cleanup();
      reject(new Error("Could not read STEP import worker response"));
    };
    try {
      const request: StepRequest = { taskId, path };
      worker.postMessage(request);
    } catch (error) {
      cleanup();
      reject(error);
    }
  });
}
