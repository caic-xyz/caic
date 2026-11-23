// Tests one turn's summary, user wait, exact details, context link, running state, section names, timeline loading, failure, and placement, previous/next controls, and ←/→ navigation.

import { afterEach, beforeEach, describe, it } from "node:test";
import { Show } from "solid-js";
import { render, screen, waitFor, within } from "@solidjs/testing-library";
import { Route, Router, useParams } from "@solidjs/router";
import userEvent from "@testing-library/user-event";
import { expect, vi } from "@tests/expect";
import { defined, historyManifest } from "@tests/history";

import type { HistoryRunningTurn, HistoryTurn, HistoryTurnTimeline, ISOTimestamp } from "@sdk/types.gen";
import { api } from "../api";
import { TaskTimelineProvider } from "../TaskTimelineContext";
import TurnDetail, { type ViewedTurn } from "./TurnDetail";

function turn(number: number): HistoryTurn {
  return {
    recordID: number,
    index: 0,
    number,
    sectionID: "1",
    startedAt: "2026-01-01T00:00:00Z" as ISOTimestamp,
    at: "2026-01-01T00:01:20Z" as ISOTimestamp,
    model: "test-model",
    durationMs: 80_000,
    durationAPIMs: 60_000,
    usage: {
      inputTokens: 1_200,
      outputTokens: 500,
      cacheCreationInputTokens: 2_000,
      cacheReadInputTokens: 40_000,
      reportedModel: "test-model",
    },
  };
}

const turns: readonly HistoryTurn[] = [
  { ...turn(1), effort: "high", waitMs: 123_000, cost: { usd: 0.12345, source: "estimated" }, contextTokens: 43_200 },
  { ...turn(2), changeStat: { files: 3, linesAdded: 14, linesDeleted: 2, binaryFiles: 0 } },
  { ...turn(3), durationAPIMs: 0, usage: { ...turn(3).usage, reasoningOutputTokens: 300 } },
];

const href = (n: number) => `/task/@abc/perf/turns/${n}`;
const contextHref = (section: string) => `/task/@abc/perf/contexts/${section}`;

function TurnRoute(props: { costUpdating: boolean; running: HistoryRunningTurn | null }) {
  const params = useParams();
  const viewed = (): ViewedTurn | undefined => {
    const completed = turns[Number(params.n) - 1];
    if (completed) return { state: "completed", turn: completed };
    return props.running?.number === Number(params.n) ? { state: "running", turn: props.running } : undefined;
  };
  return (
    <Show when={viewed()}>
      {(value) => (
        <TurnDetail
          viewed={value()}
          turns={turns}
          running={props.running}
          href={href}
          contextHref={contextHref}
          conversationHref="/task/@abc"
          costUpdating={props.costUpdating}
        />
      )}
    </Show>
  );
}

function renderTurn(n: number, costUpdating: boolean, running: HistoryRunningTurn | null) {
  window.history.replaceState(null, "", href(n));
  return render(() => (
    <Router>
      <Route
        path="/task/:taskId/perf/turns/:n"
        component={() => (
          <TaskTimelineProvider taskId="abc">
            <TurnRoute costUpdating={costUpdating} running={running} />
          </TaskTimelineProvider>
        )}
      />
    </Router>
  ));
}

function timelineOf(number: number): HistoryTurnTimeline {
  return {
    number,
    startedAt: turn(number).startedAt,
    endedAt: turn(number).at,
    durationMs: 80_000,
    modelMs: 80_000,
    spans: [{ kind: "model", startMs: 0, endMs: 80_000 }],
    omitted: [],
    calls: [],
    tools: [],
  };
}

let responses: (() => Promise<Response>)[] = [];
let requests: string[] = [];

beforeEach(() => {
  responses = [];
  requests = [];
  vi.spyOn(api, "historyEvents").mockImplementation(() => ({ close: vi.fn() }) as unknown as EventSource);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.endsWith("/manifest")) return Response.json(historyManifest([], ""));
    requests.push(url);
    const next = responses.shift();
    if (next) return next();
    const number = Number(/\/turns\/(\d+)/.exec(url)?.[1]);
    return Response.json(timelineOf(number));
  });
});

afterEach(() => vi.restoreAllMocks());

function detail(label: string): string {
  const row = screen.getByRole("rowheader", { name: label }).closest("tr");
  return row?.querySelector("td")?.textContent ?? "";
}

describe("TurnDetail", () => {
  it("summarizes the turn, the wait that followed it, and its exact details", () => {
    renderTurn(1, false, null);

    expect(screen.getByTestId("turn-summary-line")).toHaveTextContent("1:20 turn · 1:00 API · ~$0.12 · 43.7kt tokens");
    expect(screen.getByTestId("turn-user-wait")).toHaveTextContent("Then waited 2:03 for the next prompt");
    expect(detail("Model")).toBe("test-model");
    expect(detail("Effort")).toBe("high");
    expect(detail("Turn time")).toBe("80.000 s");
    expect(detail("API time")).toBe("60.000 s");
    expect(detail("New input tokens")).toBe("1,200");
    expect(detail("Cache write tokens")).toBe("2,000");
    expect(detail("Cache read tokens")).toBe("40,000");
    expect(detail("Output tokens")).toBe("500");
    expect(detail("Thinking tokens")).toBe("—");
    expect(detail("Cost")).toBe("$0.1235");
    expect(detail("Cost source")).toBe("Estimated from token prices");
    expect(detail("Context at end")).toBe("43,200 tokens");
    expect(screen.getByRole("link", { name: "Context 1" })).toHaveAttribute("href", contextHref("1"));
  });

  it("shows the running turn after the last completed one, growing from its start", async () => {
    const user = userEvent.setup();
    const running: HistoryRunningTurn = {
      number: 4,
      sectionID: "2",
      startedAt: new Date(Date.now() - 30_000).toISOString() as ISOTimestamp,
    };
    renderTurn(3, false, running);
    await screen.findByTestId("turn-timeline");
    responses.push(async () => Response.json({ ...timelineOf(4), endedAt: undefined, durationMs: 30_000 }));

    await user.click(screen.getByRole("button", { name: "Next turn" }));
    await waitFor(() => expect(window.location.pathname).toBe(href(4)));
    expect(screen.getByRole("button", { name: "Next turn" })).toBeDisabled();
    expect(screen.getByTestId("turn-summary-line")).toHaveTextContent(/^Running · 0:3\d so far/);
    expect(detail("Turn time")).toMatch(/^Running, 0:3\d so far$/);
    expect(screen.getByRole("link", { name: "Context 2" })).toHaveAttribute("href", contextHref("2"));
    expect(screen.queryByRole("rowheader", { name: "Cost" })).not.toBeInTheDocument();
    const bar = within(screen.getByTestId("turn-strip")).getByRole("link", {
      name: /^Turn 4, running, 3\d\.\d{3} s so far$/,
    });
    expect(bar).toHaveAttribute("aria-current", "page");
    await waitFor(() => expect(requests.at(-1)).toMatch(/\/history\/turns\/4\?/));
    // The loaded timeline adds its tokens to the summary.
    expect(await screen.findByTestId("turn-timeline")).toBeInTheDocument();
    expect(screen.getByTestId("turn-summary-line")).toHaveTextContent(/so far · 0t tokens$/);
  });

  // The server derived the axis end after the client clock started, so it is
  // ahead; every running figure then shows the axis end.
  for (const { name, axisMs, shown, exact } of [
    { name: "the server's axis end", axisMs: 31_600, shown: "0:32", exact: /^Turn 4, running, 31\.600 s so far$/ },
    { name: "the client clock", axisMs: 10_000, shown: "0:30", exact: /^Turn 4, running, 30\.\d{3} s so far$/ },
  ]) {
    it(`shows one running time everywhere when ${name} is later`, async () => {
      const running: HistoryRunningTurn = {
        number: 4,
        sectionID: "1",
        startedAt: new Date(Date.now() - 30_000).toISOString() as ISOTimestamp,
      };
      responses.push(async () =>
        Response.json({
          ...timelineOf(4),
          endedAt: undefined,
          durationMs: axisMs,
          spans: [{ kind: "model", startMs: 0 }],
        }),
      );
      renderTurn(4, false, running);

      expect(await screen.findByTestId("turn-timeline")).toBeInTheDocument();
      await waitFor(() => expect(screen.getByText(/^now/)).toHaveTextContent(`now ${shown}`));
      expect(screen.getByTestId("turn-summary-line")).toHaveTextContent(`Running · ${shown} so far`);
      expect(detail("Turn time")).toBe(`Running, ${shown} so far`);
      const bar = within(screen.getByTestId("turn-strip")).getByRole("link", { name: exact });
      expect(bar).toHaveAttribute("title", `Turn 4 · running · ${shown}`);
    });
  }

  it("loads the timeline, then places it, the call chart, and the tool time between the summary and the details", async () => {
    let release: () => void = () => {};
    responses.push(
      () =>
        new Promise((resolve) => {
          release = () => resolve(Response.json(timelineOf(1)));
        }),
    );
    renderTurn(1, false, null);

    expect(await screen.findByRole("status")).toHaveTextContent("Loading timeline…");
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests).toEqual([
      "/api/caic/v1/tasks/abc/history/turns/1?generation=" + historyManifest([], "").bootstrap.generation,
    ]);
    release();
    const timeline = await screen.findByTestId("turn-timeline");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(within(timeline).getAllByTestId("turn-timeline-span")).toHaveLength(1);
    const order = [
      screen.getByTestId("turn-summary-line"),
      timeline,
      screen.getByTestId("turn-calls"),
      screen.getByTestId("turn-tools"),
      screen.getByRole("region", { name: "Turn details" }),
    ];
    for (const [i, element] of order.slice(1).entries()) {
      expect(defined(order[i]).compareDocumentPosition(element) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it("reports a failed timeline load and retries it", async () => {
    const user = userEvent.setup();
    responses.push(async () =>
      Response.json({ error: { code: "internal", message: "history unavailable" } }, { status: 500 }),
    );
    renderTurn(1, false, null);

    expect(await screen.findByRole("alert")).toHaveTextContent("Timeline unavailable: history unavailable");
    expect(screen.queryByTestId("turn-calls")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByTestId("turn-timeline")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("marks the cost figures, and only them, while estimates refresh", () => {
    renderTurn(1, true, null);

    const busy = [...document.querySelectorAll('[aria-busy="true"]')].map((el) => el.textContent);
    expect(busy).toEqual(["~$0.12", "$0.1235"]);
  });

  it("names the turn without repeating the breadcrumb's heading", () => {
    renderTurn(2, false, null);

    expect(screen.getAllByRole("heading").map((heading) => heading.textContent)).toEqual(["Turn durations"]);
    expect(screen.getByRole("region", { name: "Turn 2" })).toBe(screen.getByTestId("turn-summary"));
  });

  it("keeps focus on the current bar when the arrow keys change the turn", async () => {
    const user = userEvent.setup();
    renderTurn(2, false, null);
    const strip = within(screen.getByTestId("turn-strip"));

    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Previous turn" }));
    await user.tab();
    await user.tab();
    expect(document.activeElement).toBe(strip.getByRole("link", { name: "Turn 2, 80.000 s" }));
    await user.keyboard("{ArrowRight}");
    await waitFor(() => expect(document.activeElement).toBe(strip.getByRole("link", { name: "Turn 3, 80.000 s" })));
    expect(strip.getAllByRole("link").filter((link) => link.tabIndex === 0)).toEqual([document.activeElement]);
  });

  it("omits unknown values instead of reporting zeros", () => {
    renderTurn(3, false, null);

    expect(screen.getByTestId("turn-summary-line")).toHaveTextContent(/^1:20 turn · 43.7kt tokens$/);
    expect(screen.queryByTestId("turn-user-wait")).not.toBeInTheDocument();
    expect(screen.queryByRole("rowheader", { name: "Effort" })).not.toBeInTheDocument();
    expect(screen.queryByRole("rowheader", { name: "Cost source" })).not.toBeInTheDocument();
    expect(detail("API time")).toBe("—");
    expect(detail("Thinking tokens")).toBe("300");
    expect(detail("Cost")).toBe("Unavailable");
  });

  it("disables previous and next at the ends of the task", async () => {
    const user = userEvent.setup();
    renderTurn(1, false, null);

    expect(screen.getByRole("button", { name: "Previous turn" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Next turn" }));
    await waitFor(() => expect(window.location.pathname).toBe(href(2)));
    expect(screen.getByRole("button", { name: "Previous turn" })).toBeEnabled();
    await user.click(screen.getByRole("button", { name: "Next turn" }));
    await waitFor(() => expect(window.location.pathname).toBe(href(3)));
    expect(screen.getByRole("button", { name: "Next turn" })).toBeDisabled();
    const strip = within(screen.getByTestId("turn-strip"));
    expect(strip.getByRole("link", { name: "Turn 3, 80.000 s" })).toHaveAttribute("aria-current", "page");
    expect(strip.getByRole("link", { name: "Turn 3, 80.000 s" }).className).toMatch(/_current_/);
    expect(strip.getByRole("link", { name: "Turn 1, 80.000 s" }).className).not.toMatch(/_current_/);
  });

  it("steps with the arrow keys outside editable fields", async () => {
    const user = userEvent.setup();
    renderTurn(2, false, null);
    const field = document.createElement("textarea");
    document.body.append(field);
    try {
      document.body.focus();
      await user.keyboard("{ArrowRight}");
      await waitFor(() => expect(window.location.pathname).toBe(href(3)));
      await user.keyboard("{ArrowRight}");
      expect(window.location.pathname).toBe(href(3));
      await user.keyboard("{ArrowLeft}{ArrowLeft}");
      await waitFor(() => expect(window.location.pathname).toBe(href(1)));

      field.focus();
      await user.keyboard("{ArrowRight}");
      expect(window.location.pathname).toBe(href(1));
    } finally {
      field.remove();
    }
  });
});
