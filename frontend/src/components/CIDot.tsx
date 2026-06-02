// Shared CI status dot used in TaskCard and TaskList repo headers.
import { Show } from "solid-js";
import type { CIStatus, ForgeCheck } from "@sdk/types.gen";
import Tooltip from "./Tooltip";
import styles from "./CIDot.module.css";

const DOT_CLASS: Record<string, string> = {
  pending: styles.dot_pending,
  success: styles.dot_success,
  failure: styles.dot_failure,
};

function checkSummary(status: CIStatus, checks?: ForgeCheck[]): string {
  if (!checks || checks.length === 0) return `CI: ${status}`;
  const done = checks.filter((c) => c.status === "completed").length;
  const lines = checks.map((c) => {
    const icon = c.status === "completed"
      ? (c.conclusion === "success" || c.conclusion === "neutral" || c.conclusion === "skipped" ? "\u2713" : "\u2717")
      : c.status === "in_progress" ? "\u25B6" : "\u25CB";
    const label = c.status === "completed"
      ? (c.conclusion === "success" || c.conclusion === "neutral" || c.conclusion === "skipped" ? "passed" : (c.conclusion || "failed"))
      : c.status === "in_progress" ? "running" : "queued";
    return `${icon} ${c.name}: ${label}`;
  });
  const header = `CI: ${done}/${checks.length} completed`;
  return [header, ...lines].join("\n");
}

export interface CIDotProps {
  status: CIStatus;
  checks?: ForgeCheck[];
  href?: string;
}

export default function CIDot(props: CIDotProps) {
  const cls = () => `${styles.dot} ${DOT_CLASS[props.status] ?? ""}`;
  const tip = () => checkSummary(props.status, props.checks);
  return (
    <Tooltip text={tip()}>
      <Show when={props.href} keyed fallback={<span class={cls()} />}>
        {(url) => <a class={cls()} href={url} target="_blank" rel="noopener" aria-label="CI status" onClick={(e) => e.stopPropagation()} />}
      </Show>
    </Tooltip>
  );
}
