// Tests for the shared UnifiedDiffBlock component.

import { render, screen } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import UnifiedDiffBlock from "./UnifiedDiffBlock";
import styles from "./UnifiedDiffBlock.module.css";

describe("UnifiedDiffBlock", () => {
  it("applies the line-wrap class when enabled", () => {
    const { container } = render(() => (
      <UnifiedDiffBlock diff="+long changed line" lineWrap />
    ));

    expect(container.querySelector("pre")?.className).toContain(
      styles.lineWrap,
    );
  });

  it("leaves line wrapping off by default", () => {
    const { container } = render(() => (
      <UnifiedDiffBlock diff="+long changed line" />
    ));

    expect(container.querySelector("pre")?.className).not.toContain(
      styles.lineWrap,
    );
  });

  it("trims transport headers while retaining file metadata", () => {
    const diff = [
      "diff --git a/old.ts b/new.ts",
      "old mode 100644",
      "new mode 100755",
      "similarity index 100%",
      "rename from old.ts",
      "rename to new.ts",
      "index 111..222 100755",
      "--- a/old.ts",
      "+++ b/new.ts",
      "@@ -1 +1 @@",
      "-old",
      "+new",
    ].join("\n");

    render(() => <UnifiedDiffBlock diff={diff} hideFileHeader />);

    expect(screen.getByText("old mode 100644")).toBeInTheDocument();
    expect(screen.getByText("new mode 100755")).toBeInTheDocument();
    expect(screen.getByText("similarity index 100%")).toBeInTheDocument();
    expect(screen.getByText("rename from old.ts")).toBeInTheDocument();
    expect(screen.getByText("rename to new.ts")).toBeInTheDocument();
    expect(screen.getByText("@@ -1 +1 @@")).toBeInTheDocument();
    expect(
      screen.queryByText("diff --git a/old.ts b/new.ts"),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("index 111..222 100755")).not.toBeInTheDocument();
    expect(screen.queryByText("--- a/old.ts")).not.toBeInTheDocument();
    expect(screen.queryByText("+++ b/new.ts")).not.toBeInTheDocument();
  });
});
