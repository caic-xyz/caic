// Accounts for STEP opening through the first rendered model frame without retaining file or task identities.
import type { StepWorkerTimings } from "./stepProtocol";

// Worker and window performance.now() have different origins. Only cross-context
// timestamps use epoch milliseconds; local durations retain the monotonic clock.
export function timingNow(): number {
  return performance.timeOrigin + performance.now();
}

export interface StepLoadTimingReport {
  totalMs: number;
  stages: Record<string, number>;
  accountedMs: number;
  unaccountedMs: number;
}

export class StepLoadTimings {
  private readonly started = performance.now();
  private worker: StepWorkerTimings | null = null;
  private workerStarted = 0;
  private workerReceived = 0;
  meshSetupMs = 0;
  sceneSetupMs = 0;
  firstFrameMs = 0;

  startWorker() {
    this.workerStarted = timingNow();
  }

  receiveWorker(timings: StepWorkerTimings) {
    this.workerReceived = timingNow();
    this.worker = timings;
  }

  // Called only by the still-current viewer after a visible model frame. This
  // diagnostic is deliberately not telemetry or a user-facing control.
  report() {
    if (!this.worker) return;
    const { startedAt, sentAt, ...work } = this.worker;
    // Epoch timestamps have coarser precision than monotonic durations. Their
    // subtraction can land just below zero even when intervals are equal.
    // Allow twice the timestamp's relative rounding error, capped at 0.001 ms;
    // larger negative residuals remain visible as accounting errors.
    const roundingMs = Math.min(
      0.001,
      Number.EPSILON * Math.max(startedAt, sentAt, this.workerStarted, this.workerReceived) * 2,
    );
    const normalize = (duration: number) => (duration < 0 && duration >= -roundingMs ? 0 : duration);
    const workerDuration = sentAt - startedAt;
    const stages = {
      ...work,
      workerStartupMs: normalize(startedAt - this.workerStarted),
      workerDeliveryMs: normalize(this.workerReceived - sentAt),
      workerOtherMs: normalize(workerDuration - Object.values(work).reduce((sum, value) => sum + value, 0)),
      meshSetupMs: this.meshSetupMs,
      sceneSetupMs: this.sceneSetupMs,
      firstFrameMs: this.firstFrameMs,
    };
    const totalMs = performance.now() - this.started;
    const accountedMs = Object.values(stages).reduce((sum, value) => sum + value, 0);
    // Durations are exclusive. Main-thread module loading, scheduling and gaps
    // remain explicit, rather than attributing all remaining time to rendering.
    const report: StepLoadTimingReport = { totalMs, stages, accountedMs, unaccountedMs: totalMs - accountedMs };
    // Keep only the latest completed load; failed/cancelled loads create no
    // entry. PerformanceObservers can consume each report without retaining
    // geometry, identifiers, or an ever-growing global timing history.
    performance.clearMeasures("caic.step.load");
    performance.measure("caic.step.load", { start: this.started, end: this.started + totalMs, detail: report });
  }
}
