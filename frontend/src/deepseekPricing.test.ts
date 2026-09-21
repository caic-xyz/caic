// Tests for the DeepSeek peak/off-peak pricing schedule helper.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import { deepseekPricing } from "./deepseekPricing";

function at(iso: string): number {
  return Date.parse(iso);
}

describe("deepseekPricing", () => {
  it("reports peak through a weekday morning window", () => {
    expect(deepseekPricing(at("2026-09-21T02:00:00Z"))).toEqual({
      phase: "peak",
      transitionAt: at("2026-09-21T04:00:00Z"),
    });
  });

  it("reports peak through a weekday midday window", () => {
    expect(deepseekPricing(at("2026-09-21T07:30:00Z"))).toEqual({
      phase: "peak",
      transitionAt: at("2026-09-21T10:00:00Z"),
    });
  });

  it("treats a window start as peak and its end as off-peak", () => {
    expect(deepseekPricing(at("2026-09-21T01:00:00Z")).phase).toBe("peak");
    expect(deepseekPricing(at("2026-09-21T04:00:00Z")).phase).toBe("off-peak");
  });

  it("warns within 30 minutes before a peak window", () => {
    expect(deepseekPricing(at("2026-09-21T00:30:00Z"))).toEqual({
      phase: "peak-soon",
      transitionAt: at("2026-09-21T01:00:00Z"),
    });
    expect(deepseekPricing(at("2026-09-21T05:45:00Z"))).toEqual({
      phase: "peak-soon",
      transitionAt: at("2026-09-21T06:00:00Z"),
    });
  });

  it("does not warn more than 30 minutes before a peak window", () => {
    expect(deepseekPricing(at("2026-09-21T00:29:00Z")).phase).toBe("off-peak");
    expect(deepseekPricing(at("2026-09-21T05:29:00Z")).phase).toBe("off-peak");
  });

  it("stays off-peak between windows and after the last window", () => {
    expect(deepseekPricing(at("2026-09-21T04:30:00Z")).phase).toBe("off-peak");
    expect(deepseekPricing(at("2026-09-21T12:00:00Z")).phase).toBe("off-peak");
  });

  it("is off-peak all weekend", () => {
    expect(deepseekPricing(at("2026-09-19T02:00:00Z")).phase).toBe("off-peak");
    expect(deepseekPricing(at("2026-09-20T07:00:00Z")).phase).toBe("off-peak");
  });

  it("does not warn before a peak window while a weekend is still running", () => {
    // Friday 23:59 is more than 30 minutes from Monday 01:00.
    expect(deepseekPricing(at("2026-09-18T23:59:00Z")).phase).toBe("off-peak");
    // Sunday 00:30 is more than 30 minutes from Monday 01:00.
    expect(deepseekPricing(at("2026-09-20T00:30:00Z")).phase).toBe("off-peak");
  });

  it("excludes Chinese public holidays from peak pricing", () => {
    expect(deepseekPricing(at("2026-10-01T02:00:00Z")).phase).toBe("off-peak");
    expect(deepseekPricing(at("2026-02-17T02:00:00Z")).phase).toBe("off-peak");
    expect(deepseekPricing(at("2026-02-23T02:00:00Z")).phase).toBe("off-peak");
    expect(deepseekPricing(at("2026-05-01T07:00:00Z")).phase).toBe("off-peak");
  });

  it("reports peak on the workday after a holiday range", () => {
    expect(deepseekPricing(at("2026-02-24T02:00:00Z")).phase).toBe("peak");
    expect(deepseekPricing(at("2026-10-08T02:00:00Z")).phase).toBe("peak");
  });

  it("does not warn before peak on a holiday", () => {
    expect(deepseekPricing(at("2026-05-01T00:30:00Z")).phase).toBe("off-peak");
  });
});
