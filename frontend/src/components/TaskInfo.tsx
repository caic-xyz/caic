// Full-page task metadata viewer for launch config and runtime facts.
import { For, Show, createEffect, createSignal, onCleanup, onMount, type JSX } from "solid-js";
import { useNavigate } from "@solidjs/router";
import type { TaskInfo as TaskInfoData, TaskInfoCacheMount, TaskInfoMount, TaskInfoRepo } from "@sdk/types.gen";
import { getTaskInfo } from "../api";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import styles from "./TaskInfo.module.css";

interface Props {
  taskId: string;
  repo: string;
  branch: string;
  taskPath: string;
}

function value(v: string | number | boolean | undefined): string {
  if (v === undefined || v === "" || v === 0 || v === false) return "—";
  if (v === true) return "yes";
  return String(v);
}

function formatTime(ts?: string): string {
  if (!ts) return "—";
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return ts;
  return date.toLocaleString();
}

function boolText(v?: boolean): string {
  return v ? "yes" : "no";
}

function readOnlyText(v?: boolean): string {
  return v ? "✅" : "—";
}

function compareText(a?: string, b?: string): number {
  return (a || "").localeCompare(b || "", undefined, { numeric: true, sensitivity: "base" });
}

function sortedRepos(repos?: TaskInfoRepo[]): TaskInfoRepo[] | undefined {
  return repos?.slice().sort((a, b) => compareText(a.name, b.name) || compareText(a.mountedPath, b.mountedPath));
}

function sortedMounts(mounts?: TaskInfoMount[]): TaskInfoMount[] | undefined {
  return mounts?.slice().sort((a, b) => compareText(a.hostPath, b.hostPath) || compareText(a.mountPath, b.mountPath));
}

function sortedCaches(caches?: TaskInfoCacheMount[]): TaskInfoCacheMount[] | undefined {
  return caches?.slice().sort((a, b) => compareText(a.name, b.name) || compareText(a.mountPath, b.mountPath));
}

function platformText(recorded?: string, observed?: string): string {
  if (recorded && observed && recorded !== observed) return `${recorded} requested, ${observed} running`;
  return observed || recorded || "";
}

function imageText(recorded?: string, observed?: string): string {
  if (recorded && observed && recorded !== observed) return `${recorded} requested, ${observed} running`;
  return observed || recorded || "";
}

function cpuText(recorded?: number, observed?: number): string {
  if (recorded && observed && recorded !== observed) return `${recorded} requested, ${observed} running`;
  return String(observed || recorded || "");
}

function Field(props: { label: string; value?: string | number | boolean; code?: boolean }) {
  return (
    <>
      <div class={styles.label}>{props.label}</div>
      <div class={props.code ? styles.code : styles.value}>{value(props.value)}</div>
    </>
  );
}

function Section(props: { title: string; children: JSX.Element }) {
  return (
    <section class={styles.section}>
      <h2 class={styles.sectionTitle}>{props.title}</h2>
      {props.children}
    </section>
  );
}

function MountTable(props: { mounts?: TaskInfoMount[]; empty: string }) {
  return (
    <Show when={(props.mounts?.length ?? 0) > 0} fallback={<div class={styles.empty}>{props.empty}</div>}>
      <div class={styles.tableWrap}>
        <table class={styles.table}>
          <thead>
            <tr><th class={styles.th}>Host path</th><th class={styles.th}>Runtime path</th><th class={styles.th}>Read-only</th></tr>
          </thead>
          <tbody>
            <For each={sortedMounts(props.mounts)}>
              {(m) => (
                <tr>
                  <td class={`${styles.td} ${styles.pathCell}`}>{m.hostPath || "—"}</td>
                  <td class={`${styles.td} ${styles.pathCell}`}>{m.mountPath || "—"}</td>
                  <td class={styles.td}>{readOnlyText(m.readOnly)}</td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </div>
    </Show>
  );
}

function CacheTable(props: { caches?: TaskInfoCacheMount[] }) {
  return (
    <Show when={(props.caches?.length ?? 0) > 0} fallback={<div class={styles.empty}>No caches</div>}>
      <div class={styles.tableWrap}>
        <table class={styles.table}>
          <thead>
            <tr><th class={styles.th}>Name</th><th class={styles.th}>Description</th><th class={styles.th}>Host path</th><th class={styles.th}>Runtime path</th><th class={styles.th}>Flags</th></tr>
          </thead>
          <tbody>
            <For each={sortedCaches(props.caches)}>
              {(c) => {
                const flags = () => [c.readOnly ? "read-only" : "read-write", c.shallow ? "shallow" : "full"].join(", ");
                return (
                  <tr>
                    <td class={styles.td}>{c.name || "—"}</td>
                    <td class={styles.td}>{c.description || "—"}</td>
                    <td class={`${styles.td} ${styles.pathCell}`}>{c.hostPath || "—"}</td>
                    <td class={`${styles.td} ${styles.pathCell}`}>{c.mountPath || "—"}</td>
                    <td class={styles.td}>{flags()}</td>
                  </tr>
                );
              }}
            </For>
          </tbody>
        </table>
      </div>
    </Show>
  );
}

function RepoTable(props: { repos?: TaskInfoRepo[] }) {
  return (
    <Show when={(props.repos?.length ?? 0) > 0} fallback={<div class={styles.empty}>No repositories</div>}>
      <div class={styles.tableWrap}>
        <table class={styles.table}>
          <thead>
            <tr><th class={styles.th}>Name</th><th class={styles.th}>Base</th><th class={styles.th}>Branch</th><th class={styles.th}>Host path</th><th class={styles.th}>Runtime path</th><th class={styles.th}>Remote</th></tr>
          </thead>
          <tbody>
            <For each={sortedRepos(props.repos)}>
              {(r) => (
                <tr>
                  <td class={styles.td}>{r.name}</td>
                  <td class={styles.td}>{r.baseBranch || "—"}</td>
                  <td class={styles.td}>{r.branch || "—"}</td>
                  <td class={`${styles.td} ${styles.pathCell}`}>{r.hostPath || "—"}</td>
                  <td class={`${styles.td} ${styles.pathCell}`}>{r.mountedPath || "—"}</td>
                  <td class={`${styles.td} ${styles.pathCell}`}>{r.remoteURL || "—"}</td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </div>
    </Show>
  );
}

function cacheRows(data: TaskInfoData): TaskInfoCacheMount[] | undefined {
  return (data.observed?.caches?.length ?? 0) > 0 ? data.observed?.caches : data.recorded.caches;
}

export default function TaskInfo(props: Props) {
  const navigate = useNavigate();
  const [info, setInfo] = createSignal<TaskInfoData | null>(null);
  const [error, setError] = createSignal<string | null>(null);

  createEffect(() => {
    const id = props.taskId;
    setInfo(null);
    setError(null);
    void getTaskInfo(id)
      .then(setInfo)
      .catch((err: unknown) => setError(err instanceof Error ? err.message : "Failed to load task info"));
  });

  // Escape navigates back to the task detail.
  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") navigate(props.taskPath);
    };
    document.addEventListener("keydown", onKey);
    onCleanup(() => document.removeEventListener("keydown", onKey));
  });

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button class={styles.backBtn} onClick={() => navigate(props.taskPath)} title="Back to task"><ArrowBackIcon width={20} height={20} /></button>
        <span>Info</span>
        <span class={styles.headerMeta}>
          <span class={styles.headerRepo}>{props.repo}</span>
          <span class={styles.headerBranch}>{props.branch}</span>
        </span>
      </div>
      <div class={styles.content}>
        <Show when={error()} keyed>
          {(msg) => <div class={styles.error}>{msg}</div>}
        </Show>
        <Show when={info()} keyed fallback={<Show when={!error()}><div class={styles.loading}>Loading task info...</div></Show>}>
          {(data) => (
            <>
              <Section title="Overview">
                <div class={styles.grid}>
                  <Field label="Task ID" value={data.id} code />
                  <Field label="State" value={data.recorded.state} />
                  <Field label="Started" value={formatTime(data.recorded.startedAt)} />
                  <Field label="Harness" value={data.recorded.harness} />
                  <Field label="Model" value={data.recorded.model} code />
                  <Field label="Effort" value={data.recorded.effort} />
                  <Field label="Agent version" value={data.recorded.agentVersion} code />
                  <Field label="Session ID" value={data.recorded.sessionID} code />
                </div>
              </Section>

              <Section title="Runtime">
                <div class={styles.grid}>
                  <Field label="Runtime" value={data.observed?.runtime} />
                  <Field label="Instance" value={data.recorded.runtime.id} code />
                  <Field label="State" value={data.observed?.state || data.recorded.state} />
                  <Field label="Image" value={imageText(data.recorded.baseImage, data.observed?.imageRef)} code />
                  <Field label="Image ID" value={data.observed?.imageID} code />
                  <Field label="Platform" value={platformText(data.recorded.containerPlatform, data.observed?.platform)} code />
                  <Field label="CPUs" value={cpuText(data.recorded.maxCPUs, data.observed?.cpuLimit)} />
                  <Field label="Tailscale" value={boolText(data.recorded.capabilities.tailscale)} />
                  <Field label="USB" value={boolText(data.recorded.capabilities.usb)} />
                  <Field label="Display" value={boolText(data.recorded.capabilities.display)} />
                  <Field label="Sudo" value={boolText(data.recorded.capabilities.sudo)} />
                  <Field label="GitHub token injected" value={boolText(data.recorded.capabilities.gitHubToken)} />
                </div>
              </Section>

              <Section title="Repositories"><RepoTable repos={data.recorded.repos} /></Section>
              <Section title="Caches"><CacheTable caches={cacheRows(data)} /></Section>
              <Section title="Mounted paths"><MountTable mounts={data.observed?.mounts} empty="No mounted paths" /></Section>
              <Show when={(data.warnings?.length ?? 0) > 0}>
                <Section title="Diagnostics">
                  <ul class={styles.warningList}>
                    <For each={data.warnings}>{(w) => <li>{w}</li>}</For>
                  </ul>
                </Section>
              </Show>
            </>
          )}
        </Show>
      </div>
    </div>
  );
}
