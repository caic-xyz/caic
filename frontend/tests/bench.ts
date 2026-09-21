// Bench case collector for scripts/bench.ts. Bench files import `bench` and register
// cases by module side effect; the runner imports every *.bench.ts file, drains the
// registered cases after each import, and runs them sequentially on tinybench (the same
// engine `vitest bench` used).
export interface BenchCase {
  name: string;
  file: string;
  fn: () => void | Promise<void>;
  time: number;
  warmupTime: number;
}

const cases: BenchCase[] = [];

export function bench(
  name: string,
  fn: () => void | Promise<void>,
  options?: { time?: number; warmupTime?: number },
): void {
  cases.push({
    name,
    file: "",
    fn,
    time: options?.time ?? 500,
    warmupTime: options?.warmupTime ?? 100,
  });
}

/** The runner drains the cases registered by the file it just imported. */
export function takeCases(): BenchCase[] {
  return cases.splice(0, cases.length);
}
