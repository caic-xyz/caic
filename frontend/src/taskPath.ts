// URL path helpers for task routes: slug building and ID extraction.

/** Max slug length in the URL (characters after the "+"). */
const MAX_SLUG = 80;

type TaskPathTask = {
  id: string;
  repos?: { name: string; branch?: string }[];
  title: string;
};

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

/** Build a task URL from a task DTO. */
export function taskPathForTask(task: TaskPathTask): string {
  return taskPath(task.id, task.repos?.[0]?.name ?? "", task.repos?.[0]?.branch ?? "", task.title);
}

/** Return the /task/{param} prefix of a task detail or subview pathname. */
export function taskPathFromPathname(pathname: string): string {
  return pathname.split("/").slice(0, 3).join("/");
}

/** Build the Performance overview path below a task path. */
export function taskPerformancePath(task: string): string {
  return `${task}/perf`;
}

/** Build the path of a task's 1-based turn in the Performance view. */
export function taskTurnPath(task: string, turn: number): string {
  return `${task}/perf/turns/${turn}`;
}

/** Build the path of a task's context section in the Performance view. */
export function taskContextPath(task: string, section: string): string {
  return `${task}/perf/contexts/${section}`;
}

/** Extract the task ID from a task detail or subview pathname, or null. */
export function taskIdFromPath(pathname: string): string | null {
  const prefix = "/task/@";
  if (!pathname.startsWith(prefix)) return null;
  const rest = pathname
    .slice(prefix.length)
    .replace(/\/(diff|info|perf(?:\/(?:turns|contexts)\/[^/]+)?|processes|view|vnc)$/, "");
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
