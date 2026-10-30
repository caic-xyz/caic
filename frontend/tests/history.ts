// Builds server-shaped section manifests and framed body responses for frontend fixtures.
import type {
  HistoryEvent,
  HistoryManifest,
  HistorySection,
  HistoryTurn,
  HistoryRoute,
  ISOTimestamp,
  EventMessage,
} from "@sdk/types.gen";

import { SessionGrouper } from "./sessionGrouping";
import { isChildEvent } from "../src/grouping";
import { NativeActivityTracker, BackgroundCommandTracker } from "../src/nativeSubagents";
import { ContextSkillsTracker } from "./contextSkills";
import { compareHistoryPosition } from "../src/historyPosition";

export function historyArray(events: readonly HistoryEvent[]): string {
  const lines = events.map((event) => JSON.stringify(event));
  return "[\n" + lines.join("\n,") + (lines.length ? "\n" : "") + "]\n";
}

export function historyResponse(events: readonly HistoryEvent[]): Response {
  return new Response(historyArray(events), {
    headers: { "content-type": "application/json" },
  });
}

// Builds server-shaped discovery for component fixtures; assertions still name
// the visible behavior independently of these fixture calculations.
export function historyManifest(events: readonly HistoryEvent[], initialPrompt: string): HistoryManifest {
  const messages = events.map((event) => event.message);
  const native = new NativeActivityTracker();
  const activities = native.derive(messages);
  const root = (event: EventMessage) =>
    (event.kind === "rateLimit" && event.rateLimit?.rateLimitType !== "thread_goal") || native.ownerFor(event) === null;
  const groups = new SessionGrouper();
  const sessions = groups.append(messages.filter(root));
  if (sessions.length === 0) sessions.push({ turns: [], toolCount: 0, textCount: 0, durationMs: 0 });
  const envelope = (message: EventMessage | undefined) => events.find((event) => event.message === message);
  let first = 0;
  const skills = new ContextSkillsTracker().derive("fixture", 0, messages);
  const sections: HistorySection[] = sessions.map((session, index) => {
    const lastResult = session.turns
      .flatMap((turn) => turn.groups.flatMap((group) => group.events))
      .findLast((event) => event.result && !isChildEvent(event));
    const last =
      index === sessions.length - 1 ? events.length - 1 : events.findIndex((event) => event.message === lastResult);
    const body =
      first <= last && events[first] && events[last]
        ? {
            first: { recordID: events[first]!.recordID, index: events[first]!.index },
            last: { recordID: events[last]!.recordID, index: events[last]!.index },
          }
        : undefined;
    first = last + 1;
    const boundary = envelope(session.boundaryEvent);
    const endBoundary = envelope(sessions[index + 1]?.boundaryEvent);
    return {
      id: String(index + 1),
      body,
      boundary,
      endBoundary,
      turnCount: session.turns.length,
      textCount: session.textCount,
      toolCount: session.toolCount,
      durationMs: session.durationMs,
      skills: endBoundary ? (skills.closed.get(endBoundary.message) ?? []) : skills.current,
      reportedModel: session.boundaryEvent?.init?.reportedModel,
    };
  });
  if (!sections.length) sections.push({ id: "1", turnCount: 0, textCount: 0, toolCount: 0, durationMs: 0, skills: [] });
  const turns: HistoryTurn[] = [];
  let model = "";
  let waiting = -1;
  for (const event of events) {
    const m = event.message;
    if (isChildEvent(m)) continue;
    if (m.init) model = m.init.reportedModel;
    if (m.result) {
      const section = sections.find(
        (section) =>
          section.body &&
          compareHistoryPosition(event, section.body.first) >= 0 &&
          compareHistoryPosition(event, section.body.last) <= 0,
      );
      turns.push({
        recordID: event.recordID,
        index: event.index,
        sectionID: section?.id ?? "1",
        at: new Date(m.ts).toISOString() as ISOTimestamp,
        model: m.result.usage.reportedModel || model,
        usage: m.result.usage,
        durationMs: m.result.duration * 1000,
        durationAPIMs: m.result.durationAPI * 1000,
        cost: m.result.cost,
      });
      waiting = turns.length - 1;
    }
    if (m.commitSnapshot && !m.commitSnapshot.baseline && turns.length)
      turns.at(-1)!.changeStat = m.commitSnapshot.changeStat;
    if (m.userInput && waiting >= 0) {
      const turn = turns[waiting]!;
      const wait = m.ts - Date.parse(turn.at);
      if (wait > 0) {
        turn.waitMs = wait;
        turn.waitInput = { recordID: event.recordID, index: event.index };
      }
      waiting = -1;
    }
  }
  const logs = events.filter((event) => event.message.log && !isChildEvent(event.message));
  const initial = events.find((event) => event.message.init && !isChildEvent(event.message));
  const input = events.find((event) => event.message.userInput && !isChildEvent(event.message));
  const end = Math.min(
    ...events
      .filter(
        (event) =>
          (event.message.init || event.message.userInput) && event.message.ts > 0 && !isChildEvent(event.message),
      )
      .map((event) => event.message.ts),
  );
  const routing = new Map<string, { route: HistoryRoute; recordID: number }>();
  const observeRoute = (route: HistoryRoute, recordID: number) => {
    const key = JSON.stringify(route);
    if (!routing.has(key)) routing.set(key, { route, recordID });
  };
  for (const event of events) {
    const source = event.message.source;
    if (event.message.nativeSubagent?.toolUseID)
      observeRoute(
        {
          toolUseID: event.message.nativeSubagent.toolUseID,
          namespace: source?.rootThreadID || source?.threadID || "",
          agentID: event.message.nativeSubagent.id,
        },
        event.recordID,
      );
    if (source?.delegationToolUseID && !source.agentID)
      observeRoute(
        { toolUseID: source.delegationToolUseID, namespace: source.rootThreadID || source.threadID || "" },
        event.recordID,
      );
  }
  const uniqueRoutes = [...routing.values()].map((observed) => observed.route);
  const activitySection = (kind: "nativeSubagent" | "backgroundCommand", id: string) => {
    const origin = events.findIndex(({ message }) =>
      kind === "backgroundCommand"
        ? message.backgroundCommand?.id === id
        : message.nativeSubagent?.id === id ||
          (!message.nativeSubagent &&
            !message.backgroundCommand &&
            (message.kind !== "rateLimit" || message.rateLimit?.rateLimitType === "thread_goal") &&
            native.ownerFor(message) === id),
    );
    const event = events[origin];
    if (!event) throw new Error(`Activity has no origin: ${id}`);
    const home = sections.find(
      ({ body }) =>
        body && compareHistoryPosition(event, body.first) >= 0 && compareHistoryPosition(event, body.last) <= 0,
    );
    if (!home) throw new Error(`Activity origin is outside section bodies: ${id}`);
    return home.id;
  };
  const through = events.at(-1)?.recordID ?? 0;
  const activityBounds = (kind: "nativeSubagent" | "backgroundCommand", id: string) => {
    const tools = new Set(
      events.flatMap(({ message }) =>
        message.nativeSubagent?.id === id && message.nativeSubagent.toolUseID ? [message.nativeSubagent.toolUseID] : [],
      ),
    );
    const ownershipThrough =
      kind === "backgroundCommand"
        ? 0
        : Math.max(
            0,
            ...[...routing.values()]
              .filter((observed) => tools.has(observed.route.toolUseID))
              .map((observed) => observed.recordID),
          );
    const bodyThrough = Math.max(
      ownershipThrough,
      ...events
        .filter(({ message }) =>
          kind === "backgroundCommand"
            ? message.backgroundCommand?.id === id
            : message.nativeSubagent?.id === id ||
              native.ownerFor(message) === id ||
              (message.nativeSubagent?.toolUseID && tools.has(message.nativeSubagent.toolUseID)) ||
              (message.source?.delegationToolUseID && tools.has(message.source.delegationToolUseID)),
        )
        .map((event) => event.recordID),
    );
    return { bodyThrough, ownershipThrough };
  };
  return {
    bootstrap: {
      projectionVersion: "3",
      generation: "test",
      scope: "owner",
      recordsPerChunk: 256,
      through,
      cursor: `3.test.${through}.end`,
      resourceBase: "/history/owner/3/test",
    },
    sections,
    metadata: {
      routingRevision: uniqueRoutes.length,
      routing: uniqueRoutes,
      hasInitialPrompt:
        input?.message.userInput?.text === initialPrompt &&
        !events
          .slice(0, input ? events.indexOf(input) : 0)
          .some(
            (event) =>
              !isChildEvent(event.message) &&
              ["text", "textDelta", "thinking", "thinkingDelta", "toolUse", "toolResult", "result"].includes(
                event.message.kind,
              ),
          ),
      turns,
      tools: [],
      todo: events.findLast((event) => event.message.todo && !isChildEvent(event.message))?.message.todo,
      initialInit: initial,
      initialInput: input ? { recordID: input.recordID, index: input.index } : undefined,
      setupEndedAt: Number.isFinite(end) ? (new Date(end).toISOString() as ISOTimestamp) : undefined,
      setup: logs.length
        ? {
            first: { recordID: logs[0]!.recordID, index: logs[0]!.index },
            last: { recordID: logs.at(-1)!.recordID, index: logs.at(-1)!.index },
          }
        : undefined,
      activities: [
        ...activities.map((activity) => ({
          kind: "nativeSubagent" as const,
          sectionID: activitySection("nativeSubagent", activity.id),
          id: activity.id,
          ...activityBounds("nativeSubagent", activity.id),
          status: activity.status,
          first: { recordID: 1, index: 0 },
          source: activity.source,
          toolUseID: activity.toolUseID,
          scope: activity.scope,
          groupID: activity.groupID,
          label: activity.label,
          background: activity.background,
          startedAt:
            activity.startedAt !== null ? (new Date(activity.startedAt).toISOString() as ISOTimestamp) : undefined,
          endedAt: activity.endedAt !== null ? (new Date(activity.endedAt).toISOString() as ISOTimestamp) : undefined,
          anchorAt: new Date(activity.anchorTs).toISOString() as ISOTimestamp,
        })),
        ...new BackgroundCommandTracker().derive(messages).map((activity) => ({
          kind: "backgroundCommand" as const,
          sectionID: activitySection("backgroundCommand", activity.id),
          id: activity.id,
          ...activityBounds("backgroundCommand", activity.id),
          status: activity.status,
          first: { recordID: 1, index: 0 },
          source: activity.source,
          toolUseID: activity.toolUseID,
          label: activity.label,
          exitCode: activity.exitCode ?? undefined,
          outputRef: activity.outputRef,
          startedAt:
            activity.startedAt !== null ? (new Date(activity.startedAt).toISOString() as ISOTimestamp) : undefined,
          endedAt: activity.endedAt !== null ? (new Date(activity.endedAt).toISOString() as ISOTimestamp) : undefined,
          anchorAt: new Date(activity.anchorTs).toISOString() as ISOTimestamp,
        })),
      ],
    },
  };
}

export function fixtureHistoryResponse(url: string, events: readonly HistoryEvent[], initialPrompt: string): Response {
  if (url.endsWith("/manifest")) return Response.json(historyManifest(events, initialPrompt));
  if (url.includes("/costs?")) return Response.json([]);
  const slice = url.match(/\/slice\/(\d+)\/(\d+)\/(\d+)\/(\d+|end)$/);
  if (slice)
    return historyResponse(
      events.filter(
        (event) =>
          compareHistoryPosition(event, { recordID: Number(slice[1]), index: Number(slice[2]) }) >= 0 &&
          compareHistoryPosition(event, {
            recordID: Number(slice[3]),
            index: slice[4] === "end" ? 0xffff_ffff : Number(slice[4]),
          }) <= 0,
      ),
    );
  if (url.includes("/setup/"))
    return historyResponse(events.filter((event) => event.message.log || event.message.init));
  const actor = url.match(/\/activity\/(nativeSubagent|backgroundCommand)\/([^/]+)\//);
  if (actor) {
    const id = decodeURIComponent(actor[2]!);
    const request = new URL(url, "https://fixture.invalid");
    const through = Number(request.pathname.split("/").at(-1));
    const after = {
      recordID: Number(request.searchParams.get("afterRecordID") ?? 0),
      index: Number(request.searchParams.get("afterIndex") ?? 0),
    };
    const tracker = new NativeActivityTracker();
    tracker.derive(events.map((event) => event.message));
    return historyResponse(
      events.filter(
        (event) =>
          event.recordID <= through &&
          compareHistoryPosition(event, after) > 0 &&
          (event.message.nativeSubagent?.id === id ||
            event.message.backgroundCommand?.id === id ||
            tracker.ownerFor(event.message) === id),
      ),
    );
  }
  throw new Error(`Unexpected history fixture request: ${url}`);
}

export function defined<T>(value: T | null | undefined): T {
  if (value === null || value === undefined) throw new Error("Missing fixture value");
  return value;
}
