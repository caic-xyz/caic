// Frontend error diagnostics: formats copyable browser context and original causes for failure reports.

export type ErrorReportContext = {
  occurredAt: Date;
  url: string;
  online: boolean;
  visibilityState: DocumentVisibilityState;
  serviceWorkerURL: string;
  userAgent: string;
};

function errorDetails(error: unknown): string {
  const details: string[] = [];
  const seen = new Set<unknown>();
  let current = error;
  while (!seen.has(current)) {
    seen.add(current);
    details.push(current instanceof Error && current.stack ? current.stack : String(current));
    if (!(current instanceof Error) || !Object.hasOwn(current, "cause")) return details.join("\n");
    details.push("Caused by:");
    current = current.cause;
  }
  details.push("[Circular error cause]");
  return details.join("\n");
}

export function formatErrorReport(error: unknown, context: ErrorReportContext): string {
  return [
    "caic frontend error report",
    `Occurred at: ${context.occurredAt.toISOString()}`,
    `URL: ${context.url}`,
    `Online: ${context.online ? "yes" : "no"}`,
    `Visibility: ${context.visibilityState}`,
    `Service worker: ${context.serviceWorkerURL}`,
    `User agent: ${context.userAgent}`,
    "",
    errorDetails(error),
  ].join("\n");
}

export function currentErrorReport(error: unknown): string {
  return formatErrorReport(error, {
    occurredAt: new Date(),
    url: window.location.href,
    online: navigator.onLine,
    visibilityState: document.visibilityState,
    serviceWorkerURL: navigator.serviceWorker?.controller?.scriptURL ?? "none",
    userAgent: navigator.userAgent,
  });
}
