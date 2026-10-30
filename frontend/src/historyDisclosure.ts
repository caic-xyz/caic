// Loads mounted disclosures from cached finite prefixes and advances open actors through checkpoint suffixes.
import { createEffect, createMemo, createSignal, onCleanup, untrack, type Accessor } from "solid-js";
import type { EventMessage, HistoryActivity, HistoryEvent } from "@sdk/types.gen";
import { useTaskTimeline } from "./TaskTimelineContext";

export function createHistoryDisclosure(key: Accessor<string>, load: (signal: AbortSignal) => Promise<HistoryEvent[]>) {
  const identity = createMemo(key);
  const [attempt, setAttempt] = createSignal(0);
  const [events, setEvents] = createSignal<HistoryEvent[]>([]);
  const [loading, setLoading] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  createEffect(() => {
    identity();
    attempt();
    const controller = new AbortController();
    setEvents([]);
    setLoading(true);
    setError(null);
    untrack(() => {
      void load(controller.signal).then(
        (value) => {
          if (!controller.signal.aborted) {
            setEvents(value);
            setLoading(false);
          }
        },
        (err: unknown) => {
          if (!controller.signal.aborted) {
            setError(err instanceof Error ? err.message : String(err));
            setLoading(false);
          }
        },
      );
    });
    onCleanup(() => {
      controller.abort();
      setEvents([]);
    });
  });
  return { events, loading, error, retry: () => setAttempt((previous) => previous + 1) };
}

export function createActivityHistory(activity: Accessor<HistoryActivity>) {
  const history = useTaskTimeline();
  const [messages, setMessages] = createSignal<EventMessage[]>([], { equals: false });
  const [loading, setLoading] = createSignal(true);
  const [error, setError] = createSignal<string | null>(null);
  const [attempt, setAttempt] = createSignal(0);
  let revision = "";
  let through = 0;
  let request: AbortController | null = null;
  let reading = false;
  let disposed = false;
  async function sync() {
    if (reading || disposed || through >= history.through() || error()) return;
    const controller = new AbortController();
    const value = history.manifest();
    const actor = value?.metadata.activities.find((item) => item.id === activity().id && item.kind === activity().kind);
    if (!value || !actor) return;
    request = controller;
    reading = true;
    const initial = through === 0;
    const end = initial ? actor.bodyThrough : history.through();
    // The manifest certifies that no other actor payload exists between this
    // stable body bound and its global snapshot. Only later live checkpoints
    // need suffix reads; unrelated root work can reuse the same cached body.
    const covered = initial ? value.bootstrap.through : end;
    try {
      const events = await history.readActivity(
        actor,
        end,
        through ? { recordID: through, index: 0xffff_ffff } : null,
        controller.signal,
      );
      if (disposed || controller.signal.aborted) return;
      setMessages((previous) => {
        for (const event of events) previous.push(event.message);
        return previous;
      });
      through = covered;
      setLoading(false);
    } catch (err) {
      if (!controller.signal.aborted && !disposed) {
        setError(err instanceof Error ? err.message : String(err));
        setLoading(false);
      }
    } finally {
      if (request === controller) {
        reading = false;
        if (!controller.signal.aborted) void sync();
      }
    }
  }
  createEffect(() => {
    const value = history.manifest();
    const actor = value?.metadata.activities.find((item) => item.id === activity().id && item.kind === activity().kind);
    if (!value || !actor) return;
    const identity = `${value.bootstrap.generation}:${actor.kind}:${actor.id}:${actor.ownershipThrough}:${attempt()}`;
    history.through();
    untrack(() => {
      if (identity !== revision) {
        revision = identity;
        request?.abort();
        reading = false;
        through = 0;
        setMessages([]);
        setError(null);
        setLoading(true);
      }
      void sync();
    });
  });
  onCleanup(() => {
    disposed = true;
    request?.abort();
    setMessages([]);
  });
  return { messages, loading, error, retry: () => setAttempt((previous) => previous + 1) };
}
