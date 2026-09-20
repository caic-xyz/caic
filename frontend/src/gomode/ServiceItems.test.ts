// Tests for bounded Go Mode service-item context used during browser voice setup.

import { describe, expect, it } from "vitest";

import { initialServiceContext } from "./ServiceItems";

describe("initialServiceContext", () => {
  it("formats references, attention, and omitted-item guidance", () => {
    const context = initialServiceContext(
      JSON.stringify({
        items: [
          {
            id: "1",
            reference: "Task #3",
            title: "Review plan",
            state: "has_plan",
            needsAttention: true,
          },
        ],
        moreItemsHint: "Call tasks_list and follow nextCursor until absent.",
        omittedCount: 4,
      }),
    );

    expect(context).toBe(
      "Current service items:\n" +
        "- Task #3: Review plan (has_plan, needs attention)\n" +
        "- … 4 more items omitted. Call tasks_list and follow nextCursor until absent.",
    );
  });

  it("bounds item count and reports locally omitted items", () => {
    const context = initialServiceContext(
      JSON.stringify({
        items: Array.from({ length: 25 }, (_, index) => ({
          id: `${index}`,
          title: `Item ${index}`,
          state: "active",
          needsAttention: false,
        })),
        moreItemsHint: "Use the list tool.",
      }),
    );

    expect(context).toContain("Item 19");
    expect(context).not.toContain("Item 20");
    expect(context).toContain("5 more items omitted. Use the list tool.");
    expect(context.length).toBeLessThanOrEqual(4000);
  });

  it("uses an empty baseline when advertised data is malformed", () => {
    expect(initialServiceContext('{"items":[{"id":"new","title":""}]}')).toBe("No visible service items.");
  });
});
