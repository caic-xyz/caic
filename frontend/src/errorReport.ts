// Frontend error diagnostics: formats copyable browser context for render-failure reports.

export type ErrorReportContext = {
  occurredAt: Date;
  url: string;
  online: boolean;
  visibilityState: DocumentVisibilityState;
  serviceWorkerURL: string;
  userAgent: string;
};

function errorDetails(error: unknown): string {
  if (error instanceof Error && error.stack) return error.stack;
  return String(error);
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
