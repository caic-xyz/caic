// URL path helpers for task routes: slug building and ID extraction.

/** Max slug length in the URL (characters after the "+"). */
const MAX_SLUG = 80;

/** Build a URL-safe slug from arbitrary text: lowercase, non-alnum replaced with "-", collapsed. */
export function slugify(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

/** Build the path portion for a task URL: /task/@{id}+{slug}. */
export function taskPath(id: string, repo: string, branch: string, query: string): string {
  const repoName = repo.split("/").pop() ?? repo;
  const parts = [repoName, branch, query].filter(Boolean).map(slugify).join("-");
  const slug = parts.slice(0, MAX_SLUG).replace(/-$/, "");
  return `/task/@${id}+${slug}`;
}

/** Extract the task ID from a /task/@{id}+{slug}[/diff|/processes|/vnc|/info] pathname, or null. */
export function taskIdFromPath(pathname: string): string | null {
  const prefix = "/task/@";
  if (!pathname.startsWith(prefix)) return null;
  const rest = pathname.slice(prefix.length).replace(/\/(diff|processes|vnc|info)$/, "");
  const plus = rest.indexOf("+");
  return plus === -1 ? rest : rest.slice(0, plus);
}

/** Extract the task ID from a route's :taskId param value (@{id}+{slug}), or null. */
export function taskIdFromParam(param: string | undefined): string | null {
  if (!param) return null;
  const id = param.startsWith("@") ? param.slice(1) : param;
  const plus = id.indexOf("+");
  return plus === -1 ? id : id.slice(0, plus);
}
