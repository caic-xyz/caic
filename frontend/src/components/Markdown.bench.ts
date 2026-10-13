// Benchmarks Markdown parsing and HTML sanitization for ordinary messages and the file-preview limit.
import createDOMPurify from "dompurify";
import { JSDOM } from "jsdom";
import { Marked } from "marked";
import { bench } from "@tests/bench";

const purify = createDOMPurify(new JSDOM("").window);
const marked = new Marked({ breaks: true, gfm: true });
const samples = [
  ["message", "# Notes\n\nA paragraph with [a link](https://example.com) and **emphasis**.\n\n".repeat(40)],
  ["1 MiB text file", "# Large file\n\n" + "x".repeat(1_048_562)],
] as const;

// jsdom measures relative sanitizer cost, not native-browser rendering latency.
for (const [name, source] of samples) {
  bench(
    `${name}: parse`,
    () => {
      marked.parse(source);
    },
    { time: 500, warmupTime: 100 },
  );
  bench(
    `${name}: parse and sanitize`,
    () => {
      purify.sanitize(marked.parse(source) as string, {
        USE_PROFILES: { html: true },
        FORBID_TAGS: ["form", "style"],
        FORBID_ATTR: ["form", "formaction", "formenctype", "formmethod", "formtarget"],
      });
    },
    { time: 500, warmupTime: 100 },
  );
}
