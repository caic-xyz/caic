// Gemini function schema declarations for voice mode,
// parallel to android/voice/FunctionDeclarations.kt.

type JsonSchema = Record<string, unknown>;

function stringProp(description: string): JsonSchema {
  return { type: "string", description };
}

function enumProp(description: string, values: string[]): JsonSchema {
  return { type: "string", description, enum: values };
}

function intProp(description: string): JsonSchema {
  return { type: "integer", description };
}

function boolProp(description: string): JsonSchema {
  return { type: "boolean", description };
}

function arrayProp(description: string, items: JsonSchema): JsonSchema {
  return { type: "array", description, items };
}

function objectSchema(properties: Record<string, JsonSchema>, required?: string[]): JsonSchema {
  const schema: JsonSchema = { type: "object", properties };
  if (required?.length) schema.required = required;
  return schema;
}

const emptyObjectSchema: JsonSchema = { type: "object", properties: {} };

export interface FunctionDeclaration {
  name: string;
  description: string;
  parameters: JsonSchema;
  behavior?: string;
  scheduling?: string;
}

export function buildFunctionDeclarations(
  harnesses: string[],
  repos: string[] = [],
  defaultHarness?: string,
): FunctionDeclaration[] {
  const effectiveDefault = defaultHarness ?? harnesses[0];
  const harnessDesc = effectiveDefault
    ? `Agent harness (default: ${effectiveDefault})`
    : "Agent harness to use (optional)";
  return [
    {
      name: "tasks_list",
      description: "List all current coding tasks with their status, cost, and duration.",
      parameters: emptyObjectSchema,
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_create",
      description:
        "Create a new coding task. Confirm repo and prompt with the user before calling.",
      parameters: objectSchema(
        {
          prompt: stringProp("The task description/prompt for the coding agent"),
          repos: arrayProp(
            "Repositories to work in (one or more)",
            repos.length > 0
              ? { type: "string", enum: repos }
              : { type: "string" },
          ),
          model: stringProp("Model to use (optional)"),
          harness:
            harnesses.length > 0
              ? enumProp(harnessDesc, harnesses)
              : stringProp(harnessDesc),
        },
        ["prompt", "repos"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_get_detail",
      description: "Get recent activity and status details for a task by its number.",
      parameters: objectSchema(
        { task_number: intProp("The task number, e.g. 1 for task #1") },
        ["task_number"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_send_message",
      description: "Send a text message to a waiting or asking agent by task number.",
      parameters: objectSchema(
        {
          task_number: intProp("The task number, e.g. 1 for task #1"),
          message: stringProp("The message to send to the agent"),
        },
        ["task_number", "message"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_answer_question",
      description: "Answer an agent's question by task number. The agent is in 'asking' state.",
      parameters: objectSchema(
        {
          task_number: intProp("The task number, e.g. 1 for task #1"),
          answer: stringProp("The answer to the agent's question"),
        },
        ["task_number", "answer"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_push_branch_to_remote",
      description:
        "Sync or push a task's changes to GitHub. Push to task branch (default) or squash-push to main.",
      parameters: objectSchema(
        {
          task_number: intProp("The task number, e.g. 1 for task #1"),
          force: boolProp("Force sync even with safety issues"),
          target: enumProp("Where to push: branch (default) or main", [
            "branch",
            "default",
            "main",
            "master",
          ]),
        },
        ["task_number"],
      ),
      behavior: "NON_BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_stop",
      description: "Stop a running or waiting task. The container is preserved and can be revived later.",
      parameters: objectSchema(
        { task_number: intProp("The task number, e.g. 1 for task #1") },
        ["task_number"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_purge",
      description: "Permanently delete a stopped task's container. Cannot be undone.",
      parameters: objectSchema(
        { task_number: intProp("The task number, e.g. 1 for task #1") },
        ["task_number"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_revive",
      description: "Revive a stopped task, restarting its container and agent session.",
      parameters: objectSchema(
        { task_number: intProp("The task number, e.g. 1 for task #1") },
        ["task_number"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "get_usage",
      description: "Check current API quota utilization and limits.",
      parameters: emptyObjectSchema,
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "clone_repo",
      description: "Clone a git repository by URL. Optionally specify a local path.",
      parameters: objectSchema(
        {
          url: stringProp("The git repository URL to clone"),
          path: stringProp("Local directory name (optional, derived from URL if omitted)"),
        },
        ["url"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_get_last_message_from_assistant",
      description: "Get the last text message or question from a task by its number.",
      parameters: objectSchema(
        { task_number: intProp("The task number, e.g. 1 for task #1") },
        ["task_number"],
      ),
      behavior: "NON_BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "web_search",
      description: "Search the web for a query and display the results in an embedded browser.",
      parameters: objectSchema(
        { query: stringProp("The search query") },
        ["query"],
      ),
      behavior: "NON_BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "web_fetch",
      description: "Open a URL in the embedded browser.",
      parameters: objectSchema(
        { url: stringProp("The URL to open") },
        ["url"],
      ),
      behavior: "NON_BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_fix_pr",
      description: "Inject a fix-PR command into an existing task to fix its failing PR CI in auto mode.",
      parameters: objectSchema(
        { task_number: intProp("The task number whose PR CI should be fixed") },
        ["task_number"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "bot_fix_ci",
      description: "Create a task to investigate and fix a failing CI on a repository's default branch.",
      parameters: objectSchema(
        {
          repo:
            repos.length > 0
              ? enumProp("Repository to fix CI for", repos)
              : stringProp("Repository path to fix CI for"),
        },
        ["repo"],
      ),
      behavior: "BLOCKING",
      scheduling: "INTERRUPT",
    },
  ];
}
