// DeepSeek peak/off-peak pricing schedule for the usage-badge pricing indicator.
//
// Schedule: https://api-docs.deepseek.com/quick_start/pricing/

/** Pricing phase derived from DeepSeek's published UTC peak-hour schedule. */
export type DeepseekPricingPhase = "peak" | "peak-soon" | "off-peak";

export type DeepseekPricing =
  | { phase: "peak"; transitionAt: number }
  | { phase: "peak-soon"; transitionAt: number }
  | { phase: "off-peak"; transitionAt: null };

/** How far before a peak window starts the badge warns about it. */
export const PEAK_WARNING_MS = 30 * 60 * 1000;

const MINUTE_MS = 60 * 1000;

// DeepSeek peak windows in UTC minutes-of-day: 01:00-04:00 and 06:00-10:00,
// Monday-Friday, excluding Chinese public holidays. All other hours are
// off-peak, including weekends and holidays in full.
const PEAK_WINDOWS = [
  { startMinute: 60, endMinute: 240 },
  { startMinute: 360, endMinute: 600 },
] as const;

// Chinese public holidays (State Council days off work) that DeepSeek excludes
// from peak pricing. The schedule is announced each preceding November, so add
// the following year before it begins. Years without an entry fall back to the
// weekday rule, so an unpublished year reports peak hours on holidays.
//
// The peak windows span 01:00-10:00 UTC (09:00-18:00 Beijing), so a holiday's
// Beijing calendar date matches the UTC date the window falls on.
const CHINESE_PUBLIC_HOLIDAYS: ReadonlySet<string> = new Set([
  // New Year (Jan 1-3)
  "2026-01-01",
  "2026-01-02",
  "2026-01-03",
  // Spring Festival (Feb 15-23)
  "2026-02-15",
  "2026-02-16",
  "2026-02-17",
  "2026-02-18",
  "2026-02-19",
  "2026-02-20",
  "2026-02-21",
  "2026-02-22",
  "2026-02-23",
  // Qingming (Apr 4-6)
  "2026-04-04",
  "2026-04-05",
  "2026-04-06",
  // Labor Day (May 1-5)
  "2026-05-01",
  "2026-05-02",
  "2026-05-03",
  "2026-05-04",
  "2026-05-05",
  // Dragon Boat (Jun 19-21)
  "2026-06-19",
  "2026-06-20",
  "2026-06-21",
  // Mid-Autumn (Sep 25-27)
  "2026-09-25",
  "2026-09-26",
  "2026-09-27",
  // National Day (Oct 1-7)
  "2026-10-01",
  "2026-10-02",
  "2026-10-03",
  "2026-10-04",
  "2026-10-05",
  "2026-10-06",
  "2026-10-07",
]);

interface PeakWindow {
  start: number;
  end: number;
}

// peakWindowAt returns the UTC peak window containing t, or null when t is
// off-peak.
function peakWindowAt(t: number): PeakWindow | null {
  const date = new Date(t);
  const weekday = date.getUTCDay();
  if (weekday === 0 || weekday === 6) return null;
  if (CHINESE_PUBLIC_HOLIDAYS.has(date.toISOString().slice(0, 10))) return null;

  const minuteOfDay = date.getUTCHours() * 60 + date.getUTCMinutes();
  const window = PEAK_WINDOWS.find(
    (candidate) => minuteOfDay >= candidate.startMinute && minuteOfDay < candidate.endMinute,
  );
  if (!window) return null;

  const dayStart = Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate());
  return {
    start: dayStart + window.startMinute * MINUTE_MS,
    end: dayStart + window.endMinute * MINUTE_MS,
  };
}

/**
 * Classifies DeepSeek pricing at `now`. "peak-soon" reports a peak window that
 * starts within PEAK_WARNING_MS; the included transitionAt is the phase change.
 */
export function deepseekPricing(now: number): DeepseekPricing {
  const current = peakWindowAt(now);
  if (current) return { phase: "peak", transitionAt: current.end };

  const upcoming = peakWindowAt(now + PEAK_WARNING_MS);
  if (upcoming) return { phase: "peak-soon", transitionAt: upcoming.start };

  return { phase: "off-peak", transitionAt: null };
}
