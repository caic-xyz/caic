// Tests for bounded Go Mode service-item context used during browser voice setup.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import { initialServiceContext } from "./ServiceItems";

describe("initialServiceContext", () => {
  it("formats references, attention, and omitted-item guidance", () => {
    const context = initialServiceContext(
      JSON.stringify({
        items: [
          {
            id: "1",
            reference: "Item #3",
            title: "Review plan",
            state: "pending",
            needsAttention: true,
          },
        ],
        moreItemsHint: "Call items_list and follow nextCursor until absent.",
        omittedCount: 4,
      }),
    );

    expect(context).toBe(
      "Current service items:\n" +
        "- Item #3: Review plan (pending, needs attention)\n" +
        "- … 4 more items omitted. Call items_list and follow nextCursor until absent.",
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
