// Tests STEP worker progress, transferred results, cancellation, failures, and cleanup with controlled messages.
import { afterEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { stubStepWorkers } from "../tests/step-worker";
import { createStepMesh } from "./stepMesh";
import { loadStep } from "./stepWorkerClient";

afterEach(() => vi.unstubAllGlobals());

describe("STEP worker client", () => {
  it("forwards progress until the result arrives and releases the worker and abort listener", async () => {
    const workers = stubStepWorkers();
    const controller = new AbortController();
    const progress = vi.fn();
    const loaded = loadStep("task", "/part.stpz", controller.signal, progress);
    const worker = workers[0];
    expect(worker.postMessage).toHaveBeenCalledWith({ taskId: "task", path: "/part.stpz" });
    for (const stage of ["download", "parse"] as const) {
      worker.send({ type: "progress", progress: { stage, completed: null, total: null } });
      expect(progress).toHaveBeenLastCalledWith({ stage, completed: null, total: null });
      expect(worker.terminate).not.toHaveBeenCalled();
    }
    const meshes = [createStepMesh(new Float32Array([0, 0, 0, 1, 0, 0, 0, 1, 0]), null, null, null)];
    worker.complete(meshes);
    expect(await loaded).toBe(meshes);
    controller.abort();
    expect(worker.terminate).toHaveBeenCalledTimes(1);
  });

  it("does not start a worker for a cancelled request", async () => {
    const workers = stubStepWorkers();
    const controller = new AbortController();
    controller.abort();
    await expect(loadStep("task", "/part.step", controller.signal, vi.fn())).rejects.toBe(controller.signal.reason);
    expect(workers).toHaveLength(0);
  });

  it("cancels an active parse without waiting for a result", async () => {
    const workers = stubStepWorkers();
    const controller = new AbortController();
    const loaded = loadStep("task", "/part.step", controller.signal, vi.fn());
    workers[0].send({ type: "progress", progress: { stage: "parse", completed: null, total: null } });
    controller.abort();
    await expect(loaded).rejects.toBe(controller.signal.reason);
    expect(workers[0].terminate).toHaveBeenCalledTimes(1);
  });

  it("surfaces importer errors and releases the worker", async () => {
    const workers = stubStepWorkers();
    const controller = new AbortController();
    const loaded = loadStep("task", "/broken.step", controller.signal, vi.fn());
    workers[0].send({ type: "error", message: "Invalid STEP geometry" });
    await expect(loaded).rejects.toThrow("Invalid STEP geometry");
    controller.abort();
    expect(workers[0].terminate).toHaveBeenCalledTimes(1);
  });

  for (const message of ["", "Worker script failed"]) {
    it(`handles worker startup errors with ${message ? "a message" : "no message"}`, async () => {
      const workers = stubStepWorkers();
      const loaded = loadStep("task", "/part.step", new AbortController().signal, vi.fn());
      const event = new window.ErrorEvent("error", { cancelable: true, message });
      workers[0].onerror?.(event);
      await expect(loaded).rejects.toThrow(message || "Failed to start STEP import worker");
      expect(event.defaultPrevented).toBe(true);
      expect(workers[0].terminate).toHaveBeenCalledTimes(1);
    });
  }

  it("rejects unreadable worker messages and releases the worker", async () => {
    const workers = stubStepWorkers();
    const loaded = loadStep("task", "/part.step", new AbortController().signal, vi.fn());
    workers[0].onmessageerror?.();
    await expect(loaded).rejects.toThrow("Could not read STEP import worker response");
    expect(workers[0].terminate).toHaveBeenCalledTimes(1);
  });
});
