// Parses and formats nanosecond durations used by settings APIs.

const nanosecondsPerSecond = 1_000_000_000;
const secondsPerMinute = 60;
const secondsPerHour = 60 * secondsPerMinute;

export function parseDuration(value: string): number | null {
  const input = value.trim();
  if (!input) return null;

  const part = /(\d+(?:\.\d+)?)(h|m|s)/gy;
  const unitSeconds = { h: secondsPerHour, m: secondsPerMinute, s: 1 } as const;
  let seconds = 0;
  let offset = 0;
  let count = 0;
  while (offset < input.length) {
    part.lastIndex = offset;
    const match = part.exec(input);
    if (!match) return null;
    seconds += Number(match[1]) * unitSeconds[match[2] as keyof typeof unitSeconds];
    offset = part.lastIndex;
    count++;
  }

  const nanoseconds = Math.round(seconds * nanosecondsPerSecond);
  return count > 0 && Number.isSafeInteger(nanoseconds) && nanoseconds > 0 ? nanoseconds : null;
}

export function formatDuration(nanoseconds: number): string {
  if (!Number.isSafeInteger(nanoseconds) || nanoseconds <= 0) return "0s";

  let wholeSeconds = Math.floor(nanoseconds / nanosecondsPerSecond);
  const fractionalNanoseconds = nanoseconds % nanosecondsPerSecond;
  const hours = Math.floor(wholeSeconds / secondsPerHour);
  wholeSeconds %= secondsPerHour;
  const minutes = Math.floor(wholeSeconds / secondsPerMinute);
  const seconds = wholeSeconds % secondsPerMinute;
  const parts: string[] = [];
  if (hours > 0) parts.push(`${hours}h`);
  if (minutes > 0) parts.push(`${minutes}m`);
  if (seconds > 0 || fractionalNanoseconds > 0 || parts.length === 0) {
    const fraction = fractionalNanoseconds > 0
      ? `.${String(fractionalNanoseconds).padStart(9, "0").replace(/0+$/, "")}`
      : "";
    parts.push(`${seconds}${fraction}s`);
  }
  return parts.join("");
}
