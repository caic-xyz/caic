// Reads server-owned numeric statistics and lazily refreshes prices by exact history position.
import { createEffect, createMemo, createSignal, onCleanup, untrack } from "solid-js";
import type { HistoryTurn, HistoryToolTiming, HistoryTurnEstimate } from "@sdk/types.gen";
import type { TaskHistory } from "./taskEventTimeline";
import { historyPositionKey } from "./historyPosition";

function sameFacts<T extends object>(a: T | undefined, b: T | undefined): boolean {
  if (a === b) return true;
  if (!a || !b) return false;
  for (const key in a) if (a[key] !== b[key]) return false;
  for (const key in b) if (a[key] !== b[key]) return false;
  return true;
}

function sameTurn(a: HistoryTurn, b: HistoryTurn | undefined): boolean {
  return (
    b !== undefined &&
    a.recordID === b.recordID &&
    a.index === b.index &&
    a.sectionID === b.sectionID &&
    a.at === b.at &&
    a.model === b.model &&
    a.durationMs === b.durationMs &&
    a.durationAPIMs === b.durationAPIMs &&
    a.waitMs === b.waitMs &&
    sameFacts(a.waitInput, b.waitInput) &&
    sameFacts(a.usage, b.usage) &&
    sameFacts(a.changeStat, b.changeStat) &&
    sameFacts(a.cost, b.cost)
  );
}

interface PricingSnapshot {
  resourceBase: string;
  estimates: ReadonlyMap<string, HistoryTurnEstimate>;
  pending: boolean;
  error: string | null;
}

export function createTaskAnalytics(history: TaskHistory) {
  const [pricingSnapshot, setPricingSnapshot] = createSignal<PricingSnapshot | null>(null);
  const resourceBase = createMemo(() => history.manifest()?.bootstrap.resourceBase ?? null);
  const currentPricing = createMemo(() => {
    const value = pricingSnapshot();
    return value?.resourceBase === resourceBase() ? value : null;
  });
  let request: AbortController | null = null;
  let timer: ReturnType<typeof setTimeout> | null = null;
  async function prices() {
    const resource = untrack(resourceBase);
    if (resource === null) return;
    request?.abort();
    const controller = new AbortController();
    request = controller;
    const previous = untrack(currentPricing);
    const estimates = previous?.estimates ?? new Map<string, HistoryTurnEstimate>();
    setPricingSnapshot({ resourceBase: resource, estimates, pending: true, error: previous?.error ?? null });
    try {
      const values = await history.readCosts(controller.signal);
      if (controller.signal.aborted) return;
      setPricingSnapshot({
        resourceBase: resource,
        estimates: new Map(values.map((value) => [historyPositionKey(value), value])),
        pending: false,
        error: null,
      });
    } catch (error) {
      if (!controller.signal.aborted)
        setPricingSnapshot({
          resourceBase: resource,
          estimates,
          pending: false,
          error: error instanceof Error ? error.message : String(error),
        });
    }
  }
  const boundary = createMemo(() => {
    const value = history.manifest();
    const last = value?.metadata.turns?.at(-1);
    return value ? `${value.bootstrap.resourceBase}:${last ? historyPositionKey(last) : ""}` : null;
  });
  createEffect(() => {
    const value = boundary();
    request?.abort();
    if (timer !== null) clearTimeout(timer);
    if (value === null) {
      setPricingSnapshot(null);
      return;
    }
    timer = setTimeout(() => {
      timer = null;
      void prices();
    }, 250);
    onCleanup(() => {
      request?.abort();
      if (timer !== null) clearTimeout(timer);
    });
  });
  const turns = createMemo<readonly HistoryTurn[]>((previous) => {
    const amounts = currentPricing()?.estimates;
    const next = (history.manifest()?.metadata.turns ?? []).map((turn) => {
      const estimate = amounts?.get(historyPositionKey(turn));
      return estimate ? { ...turn, model: estimate.model ?? turn.model, cost: estimate.cost ?? turn.cost } : turn;
    });
    return next.length === previous.length && next.every((turn, index) => sameTurn(turn, previous[index]))
      ? previous
      : next;
  }, []);
  const tools = createMemo<readonly HistoryToolTiming[]>((previous) => {
    const next = history.manifest()?.metadata.tools ?? [];
    return next.length === previous.length && next.every((tool, index) => sameFacts(tool, previous[index]))
      ? previous
      : next;
  }, []);
  const snapshot = createMemo(() => ({
    turns: turns(),
    tools: tools(),
    costUSD: turns().reduce((sum, turn) => sum + (turn.cost?.usd ?? 0), 0),
    costComplete: turns().every((turn) => turn.cost !== undefined),
  }));
  return {
    snapshot,
    stats: history.stats,
    loading: () => history.manifest() === null,
    pricing: () => currentPricing()?.pending ?? false,
    error: () => history.error() ?? currentPricing()?.error ?? null,
    retry: () => {
      if (history.error()) history.retry();
      else void prices();
    },
  };
}
