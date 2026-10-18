// Controls STEP worker messages and records requests and termination without running an importer.
import { vi } from "./expect";
import type { StepMessage, StepRequest } from "../src/stepProtocol";

export class StepWorkerStub {
  onmessage: ((event: MessageEvent<StepMessage>) => void) | null = null;
  onerror: ((event: ErrorEvent) => void) | null = null;
  onmessageerror: (() => void) | null = null;
  postMessage = vi.fn<(request: StepRequest) => void>();
  terminate = vi.fn<() => void>();

  send(message: StepMessage) {
    this.onmessage?.(new MessageEvent("message", { data: message }));
  }
}

export function stubStepWorkers() {
  const workers: StepWorkerStub[] = [];
  vi.stubGlobal(
    "Worker",
    vi.fn(function () {
      const worker = new StepWorkerStub();
      workers.push(worker);
      return worker;
    }),
  );
  return workers;
}
