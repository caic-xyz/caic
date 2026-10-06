// Splits leading YAML, TOML, or JSON front matter from a markdown document.

export type FrontMatterFormat = "JSON" | "TOML" | "YAML";

export interface FrontMatter {
  format: FrontMatterFormat;
  source: string;
}

export interface SplitDocument {
  frontMatter: FrontMatter | null;
  body: string;
}

const fences: { format: FrontMatterFormat; open: string; close: string[] }[] = [
  { format: "YAML", open: "---", close: ["---", "..."] },
  { format: "TOML", open: "+++", close: ["+++"] },
];

/**
 * Front matter starts at the first byte, after an optional byte-order mark.
 * An unterminated block is not front matter and stays in the body.
 */
export function splitFrontMatter(text: string): SplitDocument {
  const doc = text.startsWith("\uFEFF") ? text.slice(1) : text;
  return splitFenced(doc) ?? splitJSON(doc) ?? { frontMatter: null, body: text };
}

function splitFenced(doc: string): SplitDocument | null {
  const lines = doc.split(/\r?\n/);
  const fence = fences.find((f) => lines[0]?.trimEnd() === f.open);
  if (!fence) return null;
  const end = lines.findIndex((line, i) => i > 0 && fence.close.includes(line.trimEnd()));
  if (end < 0) return null;
  return {
    frontMatter: { format: fence.format, source: lines.slice(1, end).join("\n") },
    body: lines.slice(end + 1).join("\n"),
  };
}

// A JSON object is front matter when it is valid and ends its line.
function splitJSON(doc: string): SplitDocument | null {
  if (!doc.startsWith("{")) return null;
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = 0; i < doc.length; i++) {
    const c = doc[i];
    if (inString) {
      if (escaped) escaped = false;
      else if (c === "\\") escaped = true;
      else if (c === '"') inString = false;
    } else if (c === '"') {
      inString = true;
    } else if (c === "{") {
      depth++;
    } else if (c === "}" && --depth === 0) {
      const source = doc.slice(0, i + 1);
      const lineEnd = /^[ \t]*(?:\r?\n|$)/.exec(doc.slice(i + 1));
      if (!lineEnd) return null;
      try {
        JSON.parse(source);
      } catch {
        return null;
      }
      return { frontMatter: { format: "JSON", source }, body: doc.slice(i + 1 + lineEnd[0].length) };
    }
  }
  return null;
}
