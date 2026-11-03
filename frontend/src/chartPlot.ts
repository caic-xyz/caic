// Plot defaults shared by every chart: one class, one font, one margin rule, and
// one pointer readout.

import * as Plot from "@observablehq/plot";

// chartClass names a chart's SVG. Observable Plot also derives its legend class
// names from it, and the screenshot stabilizer finds charts by it.
export const chartClass = "data-chart";

// Plot draws tick text at this size, and labelMargin assumes it.
const chartFontSize = 10;

// basePlot returns the options every chart shares: the measured width and
// height, the class, and the transparent frame that reads as part of the page.
export function basePlot(width: number, height: number) {
  return {
    width,
    height,
    className: chartClass,
    style: { background: "transparent", color: "var(--color-text-secondary)", fontSize: `${chartFontSize}px` },
  };
}

// labelMargin returns the room a tick label needs beside the plot area. Plot
// does not widen margins for tick text, and throughput units are wider than
// percentages, so reserve a conservative seven pixels per character.
export function labelMargin(label: string): number {
  return label.length * 7 + 4;
}

// tickMargin returns the room an end tick needs when its label is centered on
// the tick: half a label plus the tick itself.
export function tickMargin(label: string | undefined): number {
  return label ? Math.ceil((label.length * 5.5) / 2) + 3 : 3;
}

export interface CrosshairDatum {
  // x is a timestamp in milliseconds, so every time series reads alike.
  x: number;
  // A null value leaves the rule alone and shows no text.
  value: number | null;
  label: string;
}

// crosshairMarks follow the pointer along x: a rule through the focused sample
// and a readout above it, kept clear of the frame edges by half a label.
export function crosshairMarks(
  points: readonly CrosshairDatum[],
  options: { color: string; plotWidth: number },
): Plot.Markish[] {
  const first = points[0];
  const last = points.at(-1);
  const span = first && last ? last.x - first.x : 0;
  const longest = points.reduce((label, point) => (point.label.length > label.length ? point.label : label), "");
  const inset = span > 0 && options.plotWidth > 0 ? (tickMargin(longest) / options.plotWidth) * span : 0;
  const readoutX = (point: CrosshairDatum) =>
    Math.min(Math.max(point.x, (first?.x ?? point.x) + inset), (last?.x ?? point.x) - inset);
  // px and py stay bound to the sample, so every pointer mark focuses the same one.
  const focus = Plot.pointerX({ px: "x", py: "value" });
  return [
    Plot.ruleX(points, { ...focus, x: "x", stroke: options.color, strokeOpacity: 0.45 }),
    Plot.text(points, {
      ...focus,
      x: readoutX,
      frameAnchor: "top",
      dy: 2,
      textAnchor: "middle",
      fill: "var(--color-text-secondary)",
      stroke: "var(--color-bg-surface)",
      strokeWidth: 3,
      text: (point: CrosshairDatum) => (point.value === null ? null : point.label),
    }),
  ];
}
