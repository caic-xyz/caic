// Vitest reporter that prints failures in full and stays silent when a run passes.
import { MinimalReporter } from "vitest/node";

export class QuietReporter extends MinimalReporter {
  reportTestSummary() {
    // Failures are reported normally; successful-run statistics add no useful signal.
  }
}
