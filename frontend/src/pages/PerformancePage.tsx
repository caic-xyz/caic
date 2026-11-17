// PerformancePage is /task/:taskId/perf: task analytics shared by the overview, its /turns/:n turn views, and its /contexts/:section context views.

import { createContext, createMemo, Show, useContext, type Accessor, type JSX } from "solid-js";
import { A, useLocation, useParams } from "@solidjs/router";

import type { Task } from "@sdk/types.gen";
import { useAppState } from "../AppState";
import { DetailPane } from "../components/Layout";
import PerformanceContent, { PerformanceBody, PerformanceSection } from "../components/PerformanceDetail";
import TaskViewHeader from "../components/TaskViewHeader";
import ContextDetail from "../components/ContextDetail";
import TurnDetail, { type ViewedTurn } from "../components/TurnDetail";
import TurnStrip, { createRunningElapsed } from "../components/TurnStrip";
import Button from "../components/Button";
import { useTaskTimeline } from "../TaskTimelineContext";
import { createTaskAnalytics } from "../createTaskAnalytics";
import { taskContextPath, taskIdFromParam, taskPathForTask, taskPerformancePath, taskTurnPath } from "../taskPath";
import styles from "./PerformancePage.module.css";

interface PerformanceState {
  analytics: ReturnType<typeof createTaskAnalytics>;
  task: () => Task | undefined;
  // Keeps the presentation query, such as full screen, across Performance links.
  overviewHref: () => string;
  turnHref: (turn: number) => string;
  contextHref: (section: string) => string;
  conversationHref: () => string;
}

const PerformanceContext = createContext<PerformanceState>();

function usePerformance(): PerformanceState {
  const value = useContext(PerformanceContext);
  if (!value) throw new Error("Performance views render inside PerformancePage");
  return value;
}

// createPerformanceLinks builds the links between Performance views and the
// conversation. The task path is a memo: task-list updates replace the task
// object far more often than they change its path, and every turn-strip bar
// reads a link, so a task-list update must not reach the bars.
export function createPerformanceLinks(
  taskId: Accessor<string>,
  task: Accessor<Task | undefined>,
  search: Accessor<string>,
) {
  const taskPath = createMemo(() => {
    const current = task();
    return current ? taskPathForTask(current) : `/task/@${taskId()}`;
  });
  return {
    overviewHref: () => `${taskPerformancePath(taskPath())}${search()}`,
    turnHref: (turn: number) => `${taskTurnPath(taskPath(), turn)}${search()}`,
    contextHref: (section: string) => `${taskContextPath(taskPath(), section)}${search()}`,
    conversationHref: taskPath,
  };
}

// turnNumber accepts only canonical positive integers, so /turns/01 is unknown.
// Context section IDs follow the same rule.
function turnNumber(param: string | undefined): number | null {
  return param !== undefined && /^[1-9]\d*$/.test(param) ? Number(param) : null;
}

function PerformanceFrame(props: { taskId: string; children?: JSX.Element }) {
  const state = useAppState();
  const params = useParams();
  const location = useLocation();
  const task = () => state.taskById(props.taskId);
  const analytics = createTaskAnalytics(useTaskTimeline());
  const value: PerformanceState = {
    analytics,
    task,
    ...createPerformanceLinks(
      () => props.taskId,
      task,
      () => location.search,
    ),
  };
  const childLabel = () => {
    if (params.section !== undefined) {
      const contexts = analytics.sections().length;
      const section = turnNumber(params.section);
      return section !== null && section <= contexts
        ? `Context ${section} of ${contexts}`
        : `Context ${params.section}`;
    }
    const raw = params.n;
    const number = turnNumber(raw);
    const total = analytics.snapshot().turns.length;
    if (number !== null && number === analytics.running()?.number) return `Turn ${number}, running`;
    return number !== null && number <= total ? `Turn ${number} of ${total}` : `Turn ${raw ?? ""}`;
  };
  const overview = () => params.n === undefined && params.section === undefined;
  // The cost-completeness notice is about all turns, so only the overview shows it.
  const costIncomplete = () =>
    overview() && !analytics.loading() && !analytics.pricing() && !analytics.snapshot().costComplete;
  // Results keep a block above the content. Loading and pricing are transient:
  // a block for them would move the content when it appears and clears, so
  // they are announced here and shown where they apply.
  const result = () => analytics.error() || costIncomplete();
  const transient = () => {
    if (analytics.loading()) return "Loading performance data…";
    return analytics.pricing() ? "Updating cost estimates…" : "";
  };

  return (
    <PerformanceContext.Provider value={value}>
      <div class={styles.container}>
        <TaskViewHeader taskPath={value.conversationHref()} label="Performance controls">
          <Show
            when={!overview()}
            fallback={
              <>
                <span>Performance</span>
                <span class={styles.headerMeta}>
                  <span class={styles.headerRepo}>{task()?.repos?.[0]?.name ?? ""}</span>
                  <span class={styles.headerBranch}>{task()?.repos?.[0]?.branch ?? ""}</span>
                </span>
              </>
            }
          >
            <nav aria-label="Breadcrumb" class={styles.breadcrumb}>
              <ol>
                <li>
                  <A href={value.overviewHref()}>Performance</A>
                </li>
                <li aria-current="page">{childLabel()}</li>
              </ol>
            </nav>
          </Show>
        </TaskViewHeader>
        <p class={styles.visuallyHidden} role="status" data-testid="performance-transient-status">
          {transient()}
        </p>
        <Show when={result()}>
          <div class={styles.status}>
            <Show when={analytics.error()}>
              {(message) => (
                <p role="alert">
                  Performance data incomplete: {message()} <Button onClick={analytics.retry}>Retry</Button>
                </p>
              )}
            </Show>
            <Show when={costIncomplete()}>
              <p role="status">Some turn costs are unavailable.</p>
            </Show>
          </div>
        </Show>
        {/* Until the history loads, the views have nothing to show, so the
            notice is the only content and clearing it moves nothing below. */}
        <Show
          when={!analytics.loading()}
          fallback={
            <p class={styles.loading} aria-hidden="true">
              Loading performance data…
            </p>
          }
        >
          {props.children}
        </Show>
      </div>
    </PerformanceContext.Provider>
  );
}

export default function PerformancePage(props: { children?: JSX.Element }) {
  const params = useParams();
  const id = () => taskIdFromParam(params.taskId);

  return (
    <Show when={id()} keyed>
      {(taskId) => (
        <DetailPane>
          <PerformanceFrame taskId={taskId}>{props.children}</PerformanceFrame>
        </DetailPane>
      )}
    </Show>
  );
}

// PerformanceOverview is the index view: the turn strip leads to each turn.
export function PerformanceOverview() {
  const perf = usePerformance();
  const snapshot = () => perf.analytics.snapshot();
  const task = perf.task;
  const runningMs = createRunningElapsed(() => perf.analytics.running());
  return (
    <PerformanceContent
      tools={snapshot().tools}
      stats={perf.analytics.stats()}
      turns={snapshot().turns}
      usage={{
        inputTokens: task()?.cumulativeInputTokens ?? 0,
        cacheWriteInputTokens: task()?.cumulativeCacheCreationInputTokens ?? 0,
        cacheReadInputTokens: task()?.cumulativeCacheReadInputTokens ?? 0,
        outputTokens: task()?.cumulativeOutputTokens ?? 0,
        costUSD: task()?.costUSD ?? snapshot().costUSD,
        costKnown: task()?.costKnown ?? snapshot().turns.some((turn) => turn.cost !== undefined),
        costComplete: task()?.costComplete ?? (snapshot().turns.length > 0 && snapshot().costComplete),
      }}
      costUpdating={perf.analytics.pricing()}
    >
      <PerformanceSection
        title={`${snapshot().turns.length} ${snapshot().turns.length === 1 ? "turn" : "turns"}`}
        testId="performance-turns"
      >
        <TurnStrip
          turns={snapshot().turns}
          running={perf.analytics.running()}
          runningMs={runningMs()}
          scaleMs={0}
          current={null}
          href={perf.turnHref}
        />
      </PerformanceSection>
    </PerformanceContent>
  );
}

// TurnPerformance is /turns/:n, the task's nth root turn counted from 1: a
// completed turn, or the running one.
export function TurnPerformance() {
  const perf = usePerformance();
  const params = useParams();
  const turns = () => perf.analytics.snapshot().turns;
  const turn = createMemo<ViewedTurn | undefined>(
    () => {
      const number = turnNumber(params.n);
      if (number === null) return undefined;
      const completed = turns().find((candidate) => candidate.number === number);
      if (completed) return { state: "completed", turn: completed };
      const running = perf.analytics.running();
      return running?.number === number ? { state: "running", turn: running } : undefined;
    },
    undefined,
    { equals: (a, b) => a?.state === b?.state && a?.turn === b?.turn },
  );
  return (
    <Show
      when={turn()}
      fallback={
        <PerformanceBody testId="turn-not-found">
          <PerformanceSection title={`Turn ${params.n ?? ""} not found`}>
            <p class={styles.notFound}>
              {turns().length === 0
                ? "This task has no completed turns yet."
                : `This task has ${turns().length} completed ${turns().length === 1 ? "turn" : "turns"}.`}{" "}
              <A href={perf.overviewHref()}>Show all turns</A>
            </p>
          </PerformanceSection>
        </PerformanceBody>
      }
    >
      {(current) => (
        <TurnDetail
          viewed={current()}
          turns={turns()}
          running={perf.analytics.running()}
          href={perf.turnHref}
          contextHref={perf.contextHref}
          conversationHref={perf.conversationHref()}
          costUpdating={perf.analytics.pricing()}
        />
      )}
    </Show>
  );
}

// ContextPerformance is /contexts/:section, one root context section of the
// task, numbered from 1.
export function ContextPerformance() {
  const perf = usePerformance();
  const params = useParams();
  const sections = () => perf.analytics.sections();
  const section = () =>
    turnNumber(params.section) === null ? undefined : sections().find((candidate) => candidate.id === params.section);
  return (
    <Show
      when={section()}
      fallback={
        <PerformanceBody testId="context-not-found">
          <PerformanceSection title={`Context ${params.section ?? ""} not found`}>
            <p class={styles.notFound}>
              {`This task has ${sections().length} ${sections().length === 1 ? "context" : "contexts"}.`}{" "}
              <A href={perf.overviewHref()}>Show all turns</A>
            </p>
          </PerformanceSection>
        </PerformanceBody>
      }
    >
      {(current) => (
        <ContextDetail
          section={current()}
          sections={sections()}
          turns={perf.analytics.snapshot().turns}
          running={perf.analytics.running()}
          href={perf.contextHref}
          turnHref={perf.turnHref}
          costUpdating={perf.analytics.pricing()}
        />
      )}
    </Show>
  );
}
