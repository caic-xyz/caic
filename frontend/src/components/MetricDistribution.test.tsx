// Tests for the measurement spread strip.

import { describe, it } from "node:test";
import { render } from "@solidjs/testing-library";
import { expect } from "@tests/expect";

import MetricDistribution from "./MetricDistribution";

function format(value: number): string {
  return `${String(value)} ms`;
}

function stripOf(container: HTMLElement): HTMLElement {
  const strip = container.querySelector<HTMLElement>('[title^="Median"]');
  if (!strip) throw new Error("spread strip did not render");
  return strip;
}

describe("MetricDistribution", () => {
  it("places the median and the 95th percentile on a scale that starts at zero", () => {
    const { container } = render(() => <MetricDistribution percentiles format={format} max={100} p50={25} p95={75} />);

    const [span, median] = Array.from(stripOf(container).children) as HTMLElement[];
    expect(span?.style.getPropertyValue("--p50-share")).toBe("0.25");
    expect(span?.style.getPropertyValue("--p95-share")).toBe("0.75");
    expect(median?.style.getPropertyValue("--p50-share")).toBe("0.25");
  });

  it("states the spread beside the numbers it describes", () => {
    const { container } = render(() => <MetricDistribution percentiles format={format} max={100} p50={25} p95={75} />);

    expect(stripOf(container)).toHaveAttribute("title", "Median 25 ms to 75 ms of a 100 ms maximum");
  });

  it("omits the strip for kinds without percentiles", () => {
    const { container, getByText } = render(() => (
      <MetricDistribution percentiles={false} format={format} max={100} p50={25} p95={75} />
    ));

    expect(container.querySelector('[title^="Median"]')).toBeNull();
    expect(getByText("—")).toBeInTheDocument();
  });

  it("omits the strip for a series without observations", () => {
    const { container } = render(() => <MetricDistribution percentiles format={format} max={0} p50={0} p95={0} />);

    expect(container.querySelector('[title^="Median"]')).toBeNull();
  });
});
