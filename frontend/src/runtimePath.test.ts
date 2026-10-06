// Tests runtime file path resolution, markdown detection, and file and viewer URLs.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import { dirname, isMarkdownPath, runtimeFilePath, taskFileURL, taskViewURL } from "./runtimePath";

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

describe("URLs", () => {
  it("encode the runtime path as a query value", () => {
    expect(taskFileURL("abc", "/home/user/a b.md")).toBe("/api/caic/v1/tasks/abc/file?path=%2Fhome%2Fuser%2Fa+b.md");
    expect(taskViewURL("abc", "/home/user/a b.md")).toBe("/task/@abc/view?path=%2Fhome%2Fuser%2Fa+b.md");
  });
});
