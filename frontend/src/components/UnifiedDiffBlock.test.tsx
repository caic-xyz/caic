// Tests for the shared UnifiedDiffBlock component.

import { render } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import UnifiedDiffBlock from "./UnifiedDiffBlock";
import styles from "./UnifiedDiffBlock.module.css";

describe("UnifiedDiffBlock", () => {
  it("applies the line-wrap class when enabled", () => {
    const { container } = render(() => <UnifiedDiffBlock diff="+long changed line" lineWrap />);

    expect(container.querySelector("pre")?.className).toContain(styles.lineWrap);
  });

  it("leaves line wrapping off by default", () => {
    const { container } = render(() => <UnifiedDiffBlock diff="+long changed line" />);

    expect(container.querySelector("pre")?.className).not.toContain(styles.lineWrap);
  });
});
