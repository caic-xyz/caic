// Gemini function call dispatch for voice mode,
// parallel to android/voice/FunctionHandlers.kt.
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
import type { TaskNumberMap } from "./TaskNumberMap";

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

function diffStatSummary(t: Task): string {
  const ds = t.diffStat;
  if (!ds?.length) return "";
  let added = 0;
  let deleted = 0;
  for (const f of ds) {
    added += f.added;
    deleted += f.deleted;
  }
  return `, +${added} -${deleted} in ${ds.length} ${ds.length === 1 ? "file" : "files"}`;
}

function taskSummaryLine(num: number, t: Task): string {
  const name = t.title || t.id;
  const base = `${num}. **${name}** — ${t.state}, ${formatElapsed(t.duration * 1000)}, ${formatCost(t.costUSD)}, ${t.harness}${diffStatSummary(t)}`;
  if (t.state === "terminated" && t.result) return `${base} — ${t.result.slice(0, RESULT_SNIPPET_MAX)}`;
  if (t.state === "failed" && t.error) return `${base} — ${t.error}`;
  return base;
}

export class FunctionHandlers {
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
