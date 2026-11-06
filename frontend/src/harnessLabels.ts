// Harness display names for product labels shown in the UI.

import type { Harness } from "@sdk/types.gen";

const displayNames: Partial<Record<Harness, string>> = {
  dsh: "DeepSeek Harness",
};

/** harnessLabel returns the product name for a harness identifier. */
export function harnessLabel(harness: string): string {
  return displayNames[harness as Harness] ?? harness;
}
