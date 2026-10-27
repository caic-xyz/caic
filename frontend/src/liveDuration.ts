// Reactive elapsed durations tick only while an observed operation is running.

import { createEffect, createSignal, onCleanup, type Accessor } from "solid-js";

export function createLiveDuration(
  startedAt: Accessor<number | null>,
  completedDuration: Accessor<number | null>,
  running: Accessor<boolean>,
): Accessor<number | null> {
  const [now, setNow] = createSignal(Date.now());
  createEffect(() => {
    if (!running() || startedAt() === null) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1_000);
    onCleanup(() => clearInterval(timer));
  });
  return () => {
    const completed = completedDuration();
    if (completed !== null) return completed;
    const start = startedAt();
    return running() && start !== null ? Math.max(0, now() - start) : null;
  };
}
