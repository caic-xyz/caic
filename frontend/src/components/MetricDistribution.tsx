// MetricDistribution plots the middle half of a measurement's spread on a fixed scale.

import { Show } from "solid-js";

import styles from "./MetricDistribution.module.css";

interface MetricDistributionProps {
  /** Whether the instrument kind has percentiles to show. */
  percentiles: boolean;
  /** The series' observed maximum, which ends the scale. */
  max: number;
  /** Formats an amount in the series' unit. */
  format: (value: number) => string;
  p50: number;
  p95: number;
}

export default function MetricDistribution(props: MetricDistributionProps) {
  // The track starts at zero and ends at the observed maximum, so rows compare
  // by position; a series without samples has no spread to place.
  const share = (value: number) => Math.min(1, Math.max(0, value / Math.max(props.max, Number.EPSILON)));
  const plottable = () => props.percentiles && props.max > 0;

  return (
    <Show when={plottable()} fallback={<span class={styles.empty}>—</span>}>
      <span
        aria-hidden="true"
        class={styles.strip}
        title={`Median ${props.format(props.p50)} to ${props.format(props.p95)} of a ${props.format(props.max)} maximum`}
      >
        <span class={styles.span} style={{ "--p50-share": share(props.p50), "--p95-share": share(props.p95) }} />
        <span class={styles.median} style={{ "--p50-share": share(props.p50) }} />
      </span>
    </Show>
  );
}
