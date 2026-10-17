// Tests STEP timing precision at epoch boundaries without masking meaningful accounting errors.
import { afterEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { StepLoadTimings } from "./stepTimings";
import type { StepLoadTimingReport } from "./stepTimings";

afterEach(() => {
  vi.restoreAllMocks();
  performance.clearMeasures("caic.step.load");
});

function report(workMs: number, workerStart: number, workerEnd: number, launch: number, receive: number) {
  let now = 0;
  vi.spyOn(performance, "now").mockImplementation(() => now);
  const load = new StepLoadTimings();
  now = launch;
  load.startWorker();
  now = receive;
  load.receiveWorker({
    startedAt: performance.timeOrigin + workerStart,
    sentAt: performance.timeOrigin + workerEnd,
    downloadMs: 0,
    decompressMs: 0,
    directParseMs: workMs,
    occtInitMs: 0,
    occtParseMs: 0,
    packingMs: 0,
  });
  now = 10;
  load.report();
  const measure = performance.getEntriesByName("caic.step.load").at(-1) as PerformanceMeasure | undefined;
  if (!measure) throw new Error("Missing STEP load timing report");
  return measure.detail as StepLoadTimingReport;
}

describe("STEP timing precision", () => {
  it("normalizes tiny negative epoch-duration residuals and keeps totals balanced", () => {
    const value = report(0.1002, 1, 1.1, 0, 5);
    expect(value.stages.workerOtherMs).toBe(0);
    expect(value.totalMs).toBeCloseTo(value.accountedMs + value.unaccountedMs, 10);
  });

  it("normalizes epoch roundoff at worker startup and delivery", () => {
    const value = report(1, 1.0001, 3.0003, 1.0003, 3.0001);
    expect(value.stages.workerStartupMs).toBe(0);
    expect(value.stages.workerDeliveryMs).toBe(0);
  });

  it("preserves positive gaps and negative residuals larger than rounding error", () => {
    expect(report(0.09, 1, 1.1, 0, 5).stages.workerOtherMs).toBeGreaterThan(0.009);
    vi.restoreAllMocks();
    expect(report(0.125, 1, 1.1, 0, 5).stages.workerOtherMs).toBeLessThan(-0.01);
  });
});
