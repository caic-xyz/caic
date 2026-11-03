// Tests runtime file path resolution, markdown and CAD detection, and file and viewer URLs.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import {
  dirname,
  isCadPath,
  isMarkdownPath,
  isStepPath,
  isStlPath,
  runtimeFilePath,
  runtimeFileTarget,
  taskRawURL,
  taskViewURL,
} from "./runtimePath";

describe("runtimeFilePath", () => {
  it("leaves web URLs, protocol-relative URLs, and anchors to the browser", () => {
    for (const href of ["https://example.com/a.md", "mailto:a@b.c", "//host/a.md", "#section", "#"]) {
      expect(runtimeFilePath(href, "/home/user")).toBeNull();
    }
  });

  it("decodes file URIs and strips fragments and line suffixes", () => {
    expect(runtimeFilePath("file:///home/user/skill%20notes.md", null)).toBe("/home/user/skill notes.md");
    expect(runtimeFilePath("/home/user/a.go:12:3", null)).toBe("/home/user/a.go");
    expect(runtimeFilePath("/home/user/a.md#intro", null)).toBe("/home/user/a.md");
  });

  it("names the line a fragment or suffix selects", () => {
    expect(runtimeFileTarget("file:///home/user/fetch.py#L1509", null)).toEqual({
      path: "/home/user/fetch.py",
      line: 1509,
    });
    expect(runtimeFileTarget("/home/user/a.go:12", null)).toEqual({ path: "/home/user/a.go", line: 12 });
    expect(runtimeFileTarget("/home/user/a.go:12:3", null)).toEqual({ path: "/home/user/a.go", line: 12 });
    expect(runtimeFileTarget("/home/user/a.go#L10-L20", null)).toEqual({ path: "/home/user/a.go", line: 10 });
    expect(runtimeFileTarget("/home/user/a.md#intro", null)).toEqual({ path: "/home/user/a.md", line: null });
    expect(runtimeFileTarget("/home/user/a.md#L0", null)).toEqual({ path: "/home/user/a.md", line: null });
    expect(runtimeFileTarget("./b.go:7", null)).toEqual({ path: "./b.go", line: 7 });
  });

  it("passes relative paths through unchanged without a base directory", () => {
    expect(runtimeFilePath("./src/App.tsx", null)).toBe("./src/App.tsx");
  });

  it("resolves relative paths against the base directory", () => {
    expect(runtimeFilePath("b.md", "/home/user/docs")).toBe("/home/user/docs/b.md");
    expect(runtimeFilePath("../README.md", "/home/user/docs")).toBe("/home/user/README.md");
    expect(runtimeFilePath("../../../../x.md", "/home/user")).toBe("/x.md");
    expect(runtimeFilePath("sub/./b.md", "/")).toBe("/sub/b.md");
    expect(runtimeFilePath("b.md", "docs")).toBe("docs/b.md");
    expect(runtimeFilePath("b.md", ".")).toBe("b.md");
    expect(runtimeFilePath("../b.md", "docs")).toBe("b.md");
    expect(runtimeFilePath("../../b.md", "docs")).toBe("../b.md");
  });

  it("keeps absolute and home paths independent of the base directory", () => {
    expect(runtimeFilePath("/etc/a.md", "/home/user/docs")).toBe("/etc/a.md");
    expect(runtimeFilePath("~/a.md", "/home/user/docs")).toBe("~/a.md");
  });

  it("treats a malformed escape as an ordinary link", () => {
    expect(runtimeFilePath("/a%zz.md", null)).toBeNull();
  });
});

describe("dirname", () => {
  it("names the containing directory", () => {
    expect(dirname("/home/user/a.md")).toBe("/home/user");
    expect(dirname("/a.md")).toBe("/");
    expect(dirname("a.md")).toBe(".");
    expect(dirname("docs/a.md")).toBe("docs");
  });
});

describe("isMarkdownPath", () => {
  it("accepts only markdown extensions", () => {
    expect(isMarkdownPath("/a/README.md")).toBe(true);
    expect(isMarkdownPath("/a/notes.MARKDOWN")).toBe(true);
    expect(isMarkdownPath("/a/notes.mdx")).toBe(false);
    expect(isMarkdownPath("/a/md")).toBe(false);
  });
});

describe("isStlPath", () => {
  it("accepts only STL extensions", () => {
    expect(isStlPath("model.stl")).toBe(true);
    expect(isStlPath("/path/to/PART.STL")).toBe(true);
    expect(isStlPath("model.stla")).toBe(false);
  });
});

describe("isStepPath", () => {
  it("accepts plain and compressed STEP and STP extensions", () => {
    expect(isStepPath("assembly.step")).toBe(true);
    expect(isStepPath("assembly.STEP")).toBe(true);
    expect(isStepPath("part.stp")).toBe(true);
    expect(isStepPath("part.STP")).toBe(true);
    for (const path of ["part.stpz", "PART.STPZ", "part.step.gz", "part.stp.gz", "part.step.zip", "part.STP.ZIP"]) {
      expect(isStepPath(path)).toBe(true);
      expect(isCadPath(path)).toBe(true);
    }
    for (const path of ["step.txt", "part.zip", "part.gz", "part.stpz.zip", "part.step.gz.txt"]) {
      expect(isStepPath(path)).toBe(false);
    }
  });
});

describe("isCadPath", () => {
  it("accepts STL, STEP, and 3MF models", () => {
    expect(isCadPath("assembly.3mf")).toBe(true);
    expect(isCadPath("/models/ASSEMBLY.3MF")).toBe(true);
    expect(isCadPath("assembly.3mf.zip")).toBe(false);
    expect(isCadPath("bracket.stl")).toBe(true);
    expect(isCadPath("bracket.step")).toBe(true);
    expect(isCadPath("bracket.stp")).toBe(true);
    expect(isCadPath("bracket.obj")).toBe(false);
  });
});

describe("URLs", () => {
  it("encode the runtime path as a query value", () => {
    expect(taskRawURL("abc", "/home/user/a b.md")).toBe("/api/caic/v1/tasks/abc/raw?path=%2Fhome%2Fuser%2Fa+b.md");
    expect(taskViewURL("abc", "/home/user/a b.md")).toBe("/task/@abc/view?path=%2Fhome%2Fuser%2Fa+b.md");
    expect(taskViewURL("abc", "/home/user/a b.md", 12)).toBe("/task/@abc/view?path=%2Fhome%2Fuser%2Fa+b.md&line=12");
  });
});

// URL resources stay within the authenticated image endpoints.
it("accepts immutable image resources and rejects external or alternate endpoint URLs", async () => {
  const { inputImageSource, inputImageViewURL } = await import("./runtimePath");
  const url = "/api/caic/v1/tasks/abc/input/def/images/0";
  expect(inputImageSource(url)).toBe(url);
  expect(inputImageSource("/api/caic/v1/tasks/abc/input/def/images/0")).not.toBeNull();
  expect(inputImageSource(`/api/caic/v1/tasks/abc/attachments/${"a".repeat(64)}`)).toBeNull();
  for (const invalid of [
    "https://other.test/image.png",
    "//other.test/image.png",
    "/api/caic/v1/tasks/abc/raw?path=secret",
    url + "?max=96",
    url + "/../x",
  ])
    expect(inputImageSource(invalid)).toBeNull();
  expect(new URL(inputImageViewURL("abc", url), "https://caic.test").searchParams.get("image")).toBe(url);
});

it("opens conversation images with downscaled pane previews", async () => {
  const { inputImageViewURL, taskAttachmentURL } = await import("./runtimePath");
  const location = "2/0/0";
  const source = `/api/caic/v1/tasks/abc/attachments/${location}`;
  const view = new URL(inputImageViewURL("abc", source), "https://caic.test");
  expect(view.searchParams.get("attachment")).toBe(location);
  expect(taskAttachmentURL("abc", location, 1280)).toBe(source + "?max=1280");
  expect(taskAttachmentURL("abc", location, null)).toBe(source);
});

it("rejects malformed log locations before constructing document image requests", async () => {
  const { taskAttachmentLocation } = await import("./runtimePath");
  expect(taskAttachmentLocation("42/3/1")).toBe("42/3/1");
  for (const value of ["0/0/0", "1/00/0", "1/-1/0", "1/0", "1/0/0/1", "../../raw", "1/0/0?max=480", "1/0/0#x"])
    expect(taskAttachmentLocation(value)).toBeNull();
});
