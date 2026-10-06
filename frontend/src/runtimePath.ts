// Resolves markdown destinations that name task-runtime files and builds their file and viewer URLs.

const markdownExtension = /\.(md|markdown)$/i;

/**
 * Returns the runtime file path a markdown destination names, or null for web
 * URLs, protocol-relative URLs, and fragment anchors. A relative path resolves
 * against baseDir when it is not null; otherwise the backend resolves it
 * against the task checkout.
 */
export function runtimeFilePath(href: string, baseDir: string | null): string | null {
  let path = href;
  if (path.startsWith("file:///")) path = path.slice(7);
  else if (/^[a-z][a-z0-9+.-]*:/i.test(path) || path.startsWith("//") || path.startsWith("#")) return null;
  path = path.replace(/#.*$/, "").replace(/:\d+(?::\d+)?$/, "");
  if (!path) return null;
  // Marked decodes neither percent-encoded spaces nor file URI paths.
  try {
    path = decodeURIComponent(path);
  } catch {
    return null;
  }
  if (baseDir !== null && !path.startsWith("/") && path !== "~" && !path.startsWith("~/")) {
    path = joinPath(baseDir, path);
  }
  return path;
}

export function isMarkdownPath(path: string): boolean {
  return markdownExtension.test(path);
}

/** Returns the directory of a runtime file path: "/" at the root, "." for a bare name. */
export function dirname(path: string): string {
  const slash = path.lastIndexOf("/");
  if (slash < 0) return ".";
  return slash === 0 ? "/" : path.slice(0, slash);
}

/** Joins and normalizes POSIX segments. Leading ".." segments of a relative result stay. */
function joinPath(dir: string, rel: string): string {
  const absolute = dir.startsWith("/");
  const out: string[] = [];
  for (const part of `${dir}/${rel}`.split("/")) {
    if (part === "" || part === ".") continue;
    if (part !== "..") out.push(part);
    else if (out.length > 0 && out[out.length - 1] !== "..") out.pop();
    else if (!absolute) out.push(part);
  }
  return (absolute ? "/" : "") + out.join("/");
}

/** The raw task-file endpoint URL for a runtime path. */
export function taskFileURL(taskId: string, path: string): string {
  return `/api/caic/v1/tasks/${encodeURIComponent(taskId)}/file?${new URLSearchParams({ path })}`;
}

/** The in-app markdown viewer route for a runtime path. */
export function taskViewURL(taskId: string, path: string): string {
  return `/task/@${encodeURIComponent(taskId)}/view?${new URLSearchParams({ path })}`;
}
