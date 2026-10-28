// Measures recent-first rendering and complete automatic backlog decoding from cached chunks.
import { bench } from "@tests/bench";
import type { HistoryEvent } from "@sdk/types.gen";
import { validateHistoryEvent } from "@sdk/validate.gen";
import { groupMessages } from "./grouping";
import { readHistoryArray } from "./historyStream";

// Many modest text/tool records match the switching baseline's transcript shape.
const events: HistoryEvent[] = Array.from({ length: 13353 }, (_, i) => ({
  recordID: i + 1,
  index: 0,
  message:
    i % 3 === 0
      ? {
          kind: "toolUse",
          ts: i + 1,
          toolUse: { toolUseID: `tool-${i}`, name: "Bash", input: { command: "git status --short" } },
        }
      : {
          kind: "text",
          ts: i + 1,
          text: { text: "A modest agent observation about the current repository and its tests." },
        },
}));
const full = JSON.stringify(events);
const recent = JSON.stringify(events.slice(-256));
const chunks: string[] = [];
for (let first = 0; first < events.length; first += 256) chunks.push(JSON.stringify(events.slice(first, first + 256)));

bench(
  "complete replay decode/group (13,353 events)",
  () => {
    const decoded = (JSON.parse(full) as unknown[]).map(validateHistoryEvent);
    groupMessages(decoded.map((event) => event.message));
  },
  { time: 500, warmupTime: 100 },
);

bench(
  "active cached chunk decode/group (256 events)",
  async () => {
    const body = new Response(recent).body;
    if (!body) throw new Error("Missing benchmark response body");
    const decoded: HistoryEvent[] = [];
    for await (const event of readHistoryArray(body, new AbortController().signal)) decoded.push(event);
    groupMessages(decoded.map((event) => event.message));
  },
  { time: 500, warmupTime: 100 },
);

bench(
  "automatic cached backlog decode/group (13,353 events)",
  async () => {
    const decoded: HistoryEvent[] = [];
    for (const chunk of chunks) {
      const body = new Response(chunk).body;
      if (!body) throw new Error("Missing benchmark response body");
      for await (const event of readHistoryArray(body, new AbortController().signal)) decoded.push(event);
    }
    groupMessages(decoded.map((event) => event.message));
  },
  { time: 500, warmupTime: 100 },
);
