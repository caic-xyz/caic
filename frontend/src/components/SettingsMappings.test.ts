// Tests POSIX destination normalization used to sort settings mounts and caches.

import { describe, it } from "node:test";
import { expect } from "../../tests/expect";
import { containerSortPath } from "./SettingsMappings";

describe("containerSortPath", () => {
  it("sorts equivalent absolute and home-relative destinations together", () => {
    expect(containerSortPath("~/cache/../.cache/tool/")).toBe("/home/user/.cache/tool");
    expect(containerSortPath("~")).toBe("/home/user");
    expect(containerSortPath("/z/../a")).toBe("/a");
    expect(containerSortPath("/a//./b/../../c/")).toBe("/c");
    expect(containerSortPath("/../../a")).toBe("/a");
    expect(containerSortPath("/")).toBe("/");
    expect(containerSortPath("")).toBe("");
  });
});
