// Tests front matter detection for YAML, TOML, and JSON, and the cases that stay body text.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import { splitFrontMatter } from "./frontMatter";

describe("splitFrontMatter", () => {
  it("splits YAML closed by --- or ...", () => {
    expect(splitFrontMatter("---\nname: a\n---\n# Title\n")).toEqual({
      frontMatter: { format: "YAML", source: "name: a" },
      body: "# Title\n",
    });
    expect(splitFrontMatter("---\nname: a\n...\nbody")).toEqual({
      frontMatter: { format: "YAML", source: "name: a" },
      body: "body",
    });
  });

  it("splits TOML", () => {
    expect(splitFrontMatter('+++\ntitle = "a"\n+++\nbody')).toEqual({
      frontMatter: { format: "TOML", source: 'title = "a"' },
      body: "body",
    });
  });

  it("splits a JSON object that ends its line", () => {
    const source = '{\n  "title": "a } b",\n  "tags": {"x": "\\"}"}\n}';
    expect(splitFrontMatter(`${source}\nbody`)).toEqual({
      frontMatter: { format: "JSON", source },
      body: "body",
    });
  });

  it("accepts a byte-order mark, CRLF lines, and empty front matter", () => {
    expect(splitFrontMatter("\uFEFF---\r\nname: a\r\n---\r\nbody")).toEqual({
      frontMatter: { format: "YAML", source: "name: a" },
      body: "body",
    });
    expect(splitFrontMatter("---\n---\nbody").frontMatter).toEqual({ format: "YAML", source: "" });
  });

  it("keeps a document without leading front matter intact", () => {
    for (const text of [
      "# Title\n---\nname: a\n---\n",
      "\n---\nname: a\n---\n",
      "---\nname: a\nno closing fence\n",
      "+++\nno closing fence\n",
      '{"a": 1} trailing words\n',
      "{not json}\n",
      '{"unterminated": ',
      "",
    ]) {
      expect(splitFrontMatter(text)).toEqual({ frontMatter: null, body: text });
    }
  });
});
