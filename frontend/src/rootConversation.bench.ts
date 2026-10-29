// Benchmarks root conversation activation and updates after large child-heavy histories.
import type { EventMessage } from "@sdk/types.gen";
import { bench } from "@tests/bench";
import { isChildEvent } from "./grouping";
import { RootConversationProjection } from "./rootConversation";

const messages: EventMessage[] = [];
let clock = 1;
function append(event: Omit<EventMessage, "ts">): void {
  messages.push({ ...event, ts: clock++ });
}
function turn(id: number, fragments: number): void {
  append({ kind: "userInput", userInput: { text: "continue" } });
  for (let i = 0; i < fragments; i++) {
    append({
      kind: "textDelta",
      source: { threadID: "root", turnID: String(id), itemID: "answer" },
      textDelta: { text: "fragment " },
    });
    for (let child = 0; child < 20; child++)
      append({
        kind: "textDelta",
        source: { agentID: "child", threadID: "child", turnID: String(id), itemID: "answer" },
        textDelta: { text: "child " },
      });
    if (i % 8 === 0) {
      const source = { threadID: "root", turnID: String(id) };
      append({ kind: "toolUse", source, toolUse: { toolUseID: `${id}-${i}`, name: "Bash", input: {} } });
      append({ kind: "toolResult", source, toolResult: { toolUseID: `${id}-${i}`, duration: 0.001 } });
    }
  }
  append({
    kind: "result",
    result: {
      subtype: "success",
      isError: false,
      result: "",
      duration: 1,
      durationAPI: 1,
      numTurns: 1,
      totalCostUSD: 0,
      usage: {
        inputTokens: 1,
        outputTokens: 1,
        cacheCreationInputTokens: 0,
        cacheReadInputTokens: 0,
        reportedModel: "m",
      },
    },
  });
}
for (let context = 0; context < 5; context++) {
  if (context) append({ kind: "system", system: { subtype: "compact_boundary" } });
  append({
    kind: "init",
    init: { sessionID: String(context), reportedModel: "m", agentVersion: "1", cwd: "/", harness: "codex" },
  });
  for (let i = 0; i < (context === 4 ? 2 : 8); i++) turn(context * 8 + i, 160);
}
const initial = messages.slice();
const completions: EventMessage[][] = [];
for (let i = 34; i < 54; i++) {
  turn(i, 40);
  completions.push(messages.slice());
}
const live: EventMessage[] = initial.slice();
const flushes: EventMessage[][] = [];
for (let i = 0; i < 200; i++) {
  live.push({
    kind: "textDelta",
    ts: clock++,
    source: { threadID: "root", turnID: "live", itemID: "answer" },
    textDelta: { text: "live " },
  });
  for (let child = 0; child < 20; child++)
    live.push({ kind: "textDelta", ts: clock++, source: { agentID: "child" }, textDelta: { text: "child " } });
  flushes.push(live.slice());
}
const keep = (event: EventMessage): boolean => !isChildEvent(event);
bench(
  "root activation (115k child-heavy messages, five contexts)",
  () => {
    new RootConversationProjection().derive(initial, keep, false);
  },
  { time: 500, warmupTime: 100 },
);
bench(
  "root history plus 20 completed turns",
  () => {
    const projection = new RootConversationProjection();
    projection.derive(initial, keep, false);
    for (const prefix of completions) projection.derive(prefix, keep, false);
  },
  { time: 500, warmupTime: 100 },
);
bench(
  "root history plus 200 live flushes",
  () => {
    const projection = new RootConversationProjection();
    projection.derive(initial, keep, false);
    for (const prefix of flushes) projection.derive(prefix, keep, false);
  },
  { time: 500, warmupTime: 100 },
);
