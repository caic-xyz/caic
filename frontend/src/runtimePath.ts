// Resolves task files and prompt images and builds viewer and resource URLs.

const markdownExtension = /\.(md|markdown)$/i;
const stlExtension = /\.stl$/i;
const stepExtension = /\.(step|stp)$/i;
const compressedStepExtension = /\.(stpz|(?:step|stp)\.(?:gz|zip))$/i;

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

export function isStlPath(path: string): boolean {
  return stlExtension.test(path);
}

export function isStepPath(path: string): boolean {
  return stepExtension.test(path) || isCompressedStepPath(path);
}

export function isCompressedStepPath(path: string): boolean {
  return compressedStepExtension.test(path);
}

export function isCadPath(path: string): boolean {
  return isStlPath(path) || isStepPath(path) || /\.3mf$/i.test(path);
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
export function taskRawURL(taskId: string, path: string): string {
  return `/api/caic/v1/tasks/${encodeURIComponent(taskId)}/raw?${new URLSearchParams({ path })}`;
}

/** The single in-app file viewer route for a runtime path. */
export function taskViewURL(taskId: string, path: string): string {
  return `/task/@${encodeURIComponent(taskId)}/view?${new URLSearchParams({ path })}`;
}

/** The maximum image dimension used for document-pane previews. */
export const attachmentPaneMaxDimension = 1280;

/** Accepts canonical numeric task-log coordinates from a document-viewer URL. */
export function taskAttachmentLocation(value: string): string | null {
  return /^[1-9][0-9]*\/(?:0|[1-9][0-9]*)\/(?:0|[1-9][0-9]*)$/.test(value) ? value : null;
}

/** The prompt image endpoint at one task-log location, optionally downscaled. */
export function taskAttachmentURL(taskId: string, location: string, maxDimension: number | null): string {
  const query = new URLSearchParams();
  if (maxDimension !== null) {
    query.set("max", String(maxDimension));
  }
  return `/api/caic/v1/tasks/${encodeURIComponent(taskId)}/attachments/${location.split("/").map(encodeURIComponent).join("/")}${query.size ? `?${query}` : ""}`;
}

/** Opens prompt images in the document viewer using their resource identity. */
export function inputImageViewURL(taskId: string, url: string): string {
  const prefix = `/api/caic/v1/tasks/${encodeURIComponent(taskId)}/attachments/`;
  const query = new URLSearchParams(url.startsWith(prefix) ? { attachment: url.slice(prefix.length) } : { image: url });
  return `/task/@${encodeURIComponent(taskId)}/view?${query}`;
}

/** Accepts only caic input-image resources as document-viewer sources. */
export function inputImageSource(url: string): string | null {
  return /^\/api\/caic\/v1\/tasks\/[a-zA-Z0-9]+\/input\/[a-zA-Z0-9]+\/images\/\d+$/.test(url) ? url : null;
}
