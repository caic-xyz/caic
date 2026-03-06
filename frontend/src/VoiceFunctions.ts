// Gemini function declarations and dispatch for voice mode.
import {
  listTasks,
  createTask,
  sendInput,
  syncTask,
  terminateTask,
  getUsage,
  cloneRepo,
  taskEvents,
} from "@sdk/api.gen";
import type { Task, EventMessage } from "@sdk/types.gen";
import { formatCost, formatElapsed } from "./formatting";

// Function declaration schema helpers

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

function objectSchema(properties: Record<string, JsonSchema>, required?: string[]): JsonSchema {
  const schema: JsonSchema = { type: "object", properties };
  if (required?.length) schema.required = required;
  return schema;
}

const emptyObjectSchema: JsonSchema = { type: "object", properties: {} };

// Function declarations

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
): FunctionDeclaration[] {
  const harnessDesc =
    harnesses.length > 0
      ? `Agent harness (default: ${harnesses[0]})`
      : "Agent harness to use (optional)";
  return [
    {
      name: "tasks_list",
      description: "List all current coding tasks with their status, cost, and duration.",
      parameters: emptyObjectSchema,
      behavior: "NON_BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_create",
      description:
        "Create a new coding task. Confirm repo and prompt with the user before calling.",
      parameters: objectSchema(
        {
          prompt: stringProp("The task description/prompt for the coding agent"),
          repo:
            repos.length > 0
              ? enumProp("Repository to work in", repos)
              : stringProp("Repository path to work in"),
          model: stringProp("Model to use (optional)"),
          harness:
            harnesses.length > 0
              ? enumProp(harnessDesc, harnesses)
              : stringProp(harnessDesc),
        },
        ["prompt", "repo"],
      ),
      behavior: "NON_BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "task_get_detail",
      description: "Get recent activity and status details for a task by its number.",
      parameters: objectSchema(
        { task_number: intProp("The task number, e.g. 1 for task #1") },
        ["task_number"],
      ),
      behavior: "NON_BLOCKING",
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
      behavior: "NON_BLOCKING",
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
      behavior: "NON_BLOCKING",
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
      name: "task_terminate",
      description: "Stop a running coding task by its number.",
      parameters: objectSchema(
        { task_number: intProp("The task number, e.g. 1 for task #1") },
        ["task_number"],
      ),
      behavior: "NON_BLOCKING",
      scheduling: "INTERRUPT",
    },
    {
      name: "get_usage",
      description: "Check current API quota utilization and limits.",
      parameters: emptyObjectSchema,
      behavior: "NON_BLOCKING",
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
  ];
}

// TaskNumberMap — bidirectional task ID ↔ 1-based number mapping

export class TaskNumberMap {
  private readonly idToNumber = new Map<string, number>();
  private readonly numberToId = new Map<number, string>();
  private nextNumber = 1;

  /** Sync with current task list. Existing tasks keep their number; new ones get the next. */
  update(tasks: Task[]): void {
    const currentIds = new Set(tasks.map((t) => t.id));
    for (const [id, num] of this.idToNumber) {
      if (!currentIds.has(id)) {
        this.idToNumber.delete(id);
        this.numberToId.delete(num);
      }
    }
    for (const task of tasks) {
      if (!this.idToNumber.has(task.id)) {
        this.idToNumber.set(task.id, this.nextNumber);
        this.numberToId.set(this.nextNumber, task.id);
        this.nextNumber++;
      }
    }
  }

  reset(): void {
    this.idToNumber.clear();
    this.numberToId.clear();
    this.nextNumber = 1;
  }

  toId(number: number): string | undefined {
    return this.numberToId.get(number);
  }

  toNumber(id: string): number | undefined {
    return this.idToNumber.get(id);
  }
}

// VoiceFunctions — dispatch Gemini tool calls to the caic API

type FunctionArgs = Record<string, unknown>;

function requireString(args: FunctionArgs, key: string): string {
  const v = args[key];
  if (typeof v !== "string") throw new Error(`Missing required parameter: ${key}`);
  return v;
}

function requireInt(args: FunctionArgs, key: string): number {
  const v = args[key];
  if (typeof v !== "number") throw new Error(`Missing required integer: ${key}`);
  return Math.floor(v);
}

function optString(args: FunctionArgs, key: string): string | undefined {
  const v = args[key];
  return typeof v === "string" ? v : undefined;
}

function optBool(args: FunctionArgs, key: string): boolean {
  return args[key] === true;
}

function textResult(message: string): Record<string, string> {
  return { result: message };
}

function errorResult(message: string): Record<string, string> {
  return { error: message };
}

const RESULT_SNIPPET_MAX = 120;

function taskSummaryLine(num: number, t: Task): string {
  const name = t.title || t.id;
  const base = `${num}. **${name}** — ${t.state}, ${formatElapsed(t.duration * 1000)}, ${formatCost(t.costUSD)}, ${t.harness}`;
  if (t.state === "terminated" && t.result) return `${base} — ${t.result.slice(0, RESULT_SNIPPET_MAX)}`;
  if (t.state === "failed" && t.error) return `${base} — ${t.error}`;
  return base;
}

export class VoiceFunctions {
  constructor(
    private readonly taskNumberMap: TaskNumberMap,
    private readonly excludedTaskIds: () => Set<string>,
  ) {}

  async handle(name: string, args: FunctionArgs): Promise<Record<string, unknown>> {
    try {
      switch (name) {
        case "tasks_list":
          return await this.handleListTasks();
        case "task_create":
          return await this.handleCreateTask(args);
        case "task_get_detail":
          return await this.handleGetTaskDetail(args);
        case "task_send_message":
          return await this.handleSendMessage(args);
        case "task_answer_question":
          return await this.handleAnswerQuestion(args);
        case "task_push_branch_to_remote":
          return await this.handleSyncTask(args);
        case "task_terminate":
          return await this.handleTerminateTask(args);
        case "get_usage":
          return await this.handleGetUsage();
        case "clone_repo":
          return await this.handleCloneRepo(args);
        case "task_get_last_message_from_assistant":
          return await this.handleGetLastMessage(args);
        default:
          return errorResult(`Unknown function: ${name}`);
      }
    } catch (e: unknown) {
      return errorResult(e instanceof Error ? e.message : "Unknown error");
    }
  }

  private async handleListTasks(): Promise<Record<string, unknown>> {
    const excluded = this.excludedTaskIds();
    const tasks = (await listTasks()).filter((t) => !excluded.has(t.id));
    if (tasks.length === 0) return textResult("No tasks running.");
    const lines = tasks.map((t) => taskSummaryLine(this.taskNumberMap.toNumber(t.id) ?? 0, t));
    return textResult(`## Tasks\n\n${lines.join("\n")}`);
  }

  private async handleCreateTask(args: FunctionArgs): Promise<Record<string, unknown>> {
    const prompt = requireString(args, "prompt");
    const repo = requireString(args, "repo");
    const model = optString(args, "model");
    const harness = optString(args, "harness") ?? "claude";
    const resp = await createTask({
      initialPrompt: { text: prompt },
      repo,
      harness,
      ...(model ? { model } : {}),
    });
    const excluded = this.excludedTaskIds();
    const tasks = (await listTasks()).filter((t) => !excluded.has(t.id));
    this.taskNumberMap.update(tasks);
    const num = this.taskNumberMap.toNumber(resp.id);
    const title = tasks.find((t) => t.id === resp.id)?.title || resp.id;
    return textResult(num !== undefined ? `Created task #${num}: ${title}` : `Created task: ${title}`);
  }

  private async handleGetTaskDetail(args: FunctionArgs): Promise<Record<string, unknown>> {
    const num = requireInt(args, "task_number");
    const taskId = this.taskNumberMap.toId(num);
    if (!taskId) return errorResult("Unknown task number");
    const tasks = await listTasks();
    const t = tasks.find((x) => x.id === taskId);
    if (!t) return errorResult(`Task #${num} not found`);
    const shortName = t.title || t.id;
    const lines = [
      `## Task #${num}: ${shortName}`,
      "",
      `State: ${t.state}  Elapsed: ${formatElapsed(t.duration * 1000)}  Cost: ${formatCost(t.costUSD)}`,
    ];
    if (t.state === "terminated" && t.result) lines.push(`**Result:** ${t.result}`);
    if (t.state === "failed" && t.error) lines.push(`**Error:** ${t.error}`);
    if (t.diffStat?.length) lines.push(`**Changed:** ${t.diffStat.map((d) => d.path).join(", ")}`);
    return textResult(lines.join("\n").trim());
  }

  private async handleSendMessage(args: FunctionArgs): Promise<Record<string, unknown>> {
    const num = requireInt(args, "task_number");
    const taskId = this.taskNumberMap.toId(num);
    if (!taskId) return errorResult("Unknown task number");
    await sendInput(taskId, { prompt: { text: requireString(args, "message") } });
    return textResult(`Sent message to task #${num}.`);
  }

  private async handleAnswerQuestion(args: FunctionArgs): Promise<Record<string, unknown>> {
    const num = requireInt(args, "task_number");
    const taskId = this.taskNumberMap.toId(num);
    if (!taskId) return errorResult("Unknown task number");
    await sendInput(taskId, { prompt: { text: requireString(args, "answer") } });
    return textResult(`Answered task #${num}.`);
  }

  private async handleSyncTask(args: FunctionArgs): Promise<Record<string, unknown>> {
    const num = requireInt(args, "task_number");
    const taskId = this.taskNumberMap.toId(num);
    if (!taskId) return errorResult("Unknown task number");
    const force = optBool(args, "force");
    const targetRaw = optString(args, "target");
    const target = targetRaw === "main" || targetRaw === "master" ? "default" : targetRaw;
    const resp = await syncTask(taskId, {
      ...(force ? { force } : {}),
      ...(target ? { target } : {}),
    });
    const verb = target === "default" ? `Pushed task #${num} to main` : `Synced task #${num}`;
    if (!resp.safetyIssues?.length) return textResult(`${verb}.`);
    const issueLines = resp.safetyIssues
      .map((i) => `- **${i.kind}** ${i.file}: ${i.detail}`)
      .join("\n");
    return textResult(`${verb} with safety issues:\n${issueLines}`);
  }

  private async handleTerminateTask(args: FunctionArgs): Promise<Record<string, unknown>> {
    const num = requireInt(args, "task_number");
    const taskId = this.taskNumberMap.toId(num);
    if (!taskId) return errorResult("Unknown task number");
    await terminateTask(taskId);
    return textResult(`Terminated task #${num}.`);
  }

  private async handleGetUsage(): Promise<Record<string, unknown>> {
    const usage = await getUsage();
    const pct = (v: number) => `${Math.floor(v)}%`;
    const lines = [
      `5-hour window: ${pct(usage.fiveHour.utilization)} used, resets ${usage.fiveHour.resetsAt}`,
      `7-day window: ${pct(usage.sevenDay.utilization)} used, resets ${usage.sevenDay.resetsAt}`,
    ];
    if (usage.extraUsage.isEnabled) {
      const usedDollars = Math.floor(usage.extraUsage.usedCredits / 100);
      const limitDollars = Math.floor(usage.extraUsage.monthlyLimit / 100);
      lines.push(`Extra usage: $${usedDollars} of $${limitDollars} monthly limit used`);
    }
    return textResult(lines.join("\n"));
  }

  private async handleCloneRepo(args: FunctionArgs): Promise<Record<string, unknown>> {
    const url = requireString(args, "url");
    const path = optString(args, "path");
    const repo = await cloneRepo({ url, ...(path ? { path } : {}) });
    return textResult(`Cloned **${repo.path}** (base: ${repo.baseBranch}).`);
  }

  private handleGetLastMessage(args: FunctionArgs): Promise<Record<string, unknown>> {
    const num = requireInt(args, "task_number");
    const taskId = this.taskNumberMap.toId(num);
    if (!taskId) return Promise.resolve(errorResult("Unknown task number"));

    return new Promise((resolve) => {
      const collected: EventMessage[] = [];
      let settled = false;
      let historyTimer: ReturnType<typeof setTimeout> | null = null;

      const settle = () => {
        if (settled) return;
        settled = true;
        if (historyTimer !== null) clearTimeout(historyTimer);
        es.close();
        const lastResult = [...collected].reverse().find((e) => e.kind === "result");
        const lastAsk = [...collected].reverse().find((e) => e.kind === "ask");
        const lastText = [...collected].reverse().find((e) => e.kind === "text");
        if (lastResult?.result?.result) {
          resolve(textResult(`Task #${num} result: ${lastResult.result.result}`));
        } else if (lastAsk?.ask?.questions?.[0]) {
          const q = lastAsk.ask.questions[0];
          const opts = q.options.map((o) => o.label).join(", ");
          resolve(
            textResult(
              `Task #${num} is asking: ${q.question}${opts ? ` Options: ${opts}` : ""}`,
            ),
          );
        } else if (lastText?.text?.text) {
          resolve(textResult(`Last message from task #${num}: ${lastText.text.text}`));
        } else {
          resolve(textResult(`No messages from task #${num} yet.`));
        }
      };

      const es = taskEvents(taskId, (event) => {
        collected.push(event);
        if (event.kind === "init") {
          // Wait 1s after init to let the replay stream in.
          if (historyTimer !== null) clearTimeout(historyTimer);
          historyTimer = setTimeout(settle, 1000);
        }
      });

      // Fallback: settle after 5s regardless.
      setTimeout(settle, 5000);
    });
  }
}
