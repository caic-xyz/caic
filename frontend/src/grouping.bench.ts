// Benchmarks bounded text/tool streaming and compact concurrent transcript display.
import type { EventMessage } from "@sdk/types.gen";
import { bench } from "@tests/bench";
import { groupMessages, IncrementalMessageGrouper } from "./grouping";
import { NativeActivityTracker } from "./nativeSubagents";

const concurrent: EventMessage[] = Array.from({ length: 10000 }, (_, i) => {
  const source = {
    threadID: i % 2 === 0 ? "parent" : "child",
    rootThreadID: "parent",
    turnID: "turn",
    itemID: `item-${Math.floor(i / 1000)}`,
  };
  return i % 2 === 0
    ? { kind: "thinkingDelta", ts: i + 1, source, thinkingDelta: { text: "Consider " } }
    : { kind: "textDelta", ts: i + 1, source, textDelta: { text: "fragment " } };
});
const legacy: EventMessage[] = concurrent.map((event) =>
  event.kind === "thinkingDelta"
    ? { ...event, source: undefined, thinkingDelta: { text: "Consider " } }
    : { ...event, source: undefined, textDelta: { text: "fragment " } },
);
const batches = Array.from({ length: concurrent.length / 50 }, (_, i) => concurrent.slice(0, (i + 1) * 50));
const singleMessage: EventMessage[] = Array.from({ length: 10000 }, (_, i) => ({
  kind: "textDelta",
  ts: i + 1,
  source: { threadID: "child", rootThreadID: "parent", turnID: "turn", itemID: "answer" },
  textDelta: {
    text: "fragment ",
  },
}));
const toolStream: EventMessage[] = [
  {
    kind: "toolUse",
    ts: 1,
    source: { threadID: "child", turnID: "turn", agentID: "native:child" },
    toolUse: { toolUseID: "call", name: "Bash", input: {} },
  },
  ...Array.from({ length: 10000 }, (_, i): EventMessage => ({
    kind: "toolOutputDelta",
    ts: i + 2,
    source: { threadID: "child", turnID: "turn", agentID: "native:child" },
    toolOutputDelta: { toolUseID: "call", delta: "output " },
  })),
];
bench(
  "tool output live (10k individual fragments)",
  () => {
    const grouper = new IncrementalMessageGrouper();
    const messages: EventMessage[] = [];
    for (const event of toolStream) {
      messages.push(event);
      grouper.group(messages);
    }
  },
  { time: 500, warmupTime: 100 },
);

bench(
  "concurrent full history (10k fragments)",
  () => {
    groupMessages(concurrent);
  },
  { time: 500, warmupTime: 100 },
);
bench(
  "legacy full history (10k fragments)",
  () => {
    groupMessages(legacy);
  },
  { time: 500, warmupTime: 100 },
);
bench(
  "concurrent live (10k fragments, batches of 50)",
  () => {
    const grouper = new IncrementalMessageGrouper();
    for (const batch of batches) grouper.group(batch);
  },
  { time: 500, warmupTime: 100 },
);
bench(
  "single message live (10k individual fragments)",
  () => {
    const grouper = new IncrementalMessageGrouper();
    const messages: EventMessage[] = [];
    for (const event of singleMessage) {
      messages.push(event);
      grouper.group(messages);
    }
  },
  { time: 500, warmupTime: 100 },
);

const summaryPrefix: EventMessage[] = Array.from({ length: 1000 }, (_, i) => ({
  kind: "toolUse",
  ts: i + 1,
  source: { threadID: "child", turnID: "turn", agentID: "native:child" },
  toolUse: { toolUseID: i === 0 ? "call" : `call-${i}`, name: "Bash", input: {} },
}));
bench(
  "compact child summary (1k tools, 10k output, batches of 50)",
  () => {
    const tracker = new NativeActivityTracker();
    const messages = [...summaryPrefix];
    tracker.derive(messages);
    for (let i = 1; i < toolStream.length; i += 50) {
      messages.push(...toolStream.slice(i, i + 50));
      tracker.derive(messages);
    }
  },
  { time: 500, warmupTime: 100 },
);
