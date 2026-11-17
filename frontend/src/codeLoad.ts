// Distinguishes retryable native module-fetch failures from imports that require a fresh document.

export class CodeLoadError extends Error {
  constructor(cause: unknown) {
    super("Page content could not be loaded.", { cause });
    this.name = "CodeLoadError";
  }

  get canRetry(): boolean {
    // import() exposes no structured fetch/evaluation failure category. Recognize
    // known browser fetch TypeErrors only; unknown failures conservatively reload.
    // A module can throw the same text itself, so this distinction is best-effort.
    if (!(this.cause instanceof TypeError)) return false;
    return (
      /^(?:Failed to fetch dynamically imported module|error loading dynamically imported module)(?:: .+)?$/.test(
        this.cause.message,
      ) ||
      this.cause.message === "Importing a module script failed" ||
      this.cause.message === "Importing a module script failed."
    );
  }
}

// Tag only the import, leaving later component-render failures ordinary. The
// installed Solid.lazy resets rejected loaders and permits native fetch retries.
// Evaluation failures stay in the browser module map, and Vite retains failed
// CSS-preload deduplication, so those failures need a fresh document.
// https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Operators/import
export async function loadCode<T>(promise: Promise<T>): Promise<T> {
  try {
    return await promise;
  } catch (cause) {
    throw new CodeLoadError(cause);
  }
}
