// Full-page process list viewer for a task's container.
import { createSignal, createEffect, For, Show } from "solid-js";
import { useNavigate } from "@solidjs/router";
import type { ProcessInfo } from "@sdk/types.gen";
import { getTaskProcesses, signalProcess } from "./api";
import ArrowBackIcon from "@material-symbols/svg-400/outlined/arrow_back.svg?solid";
import styles from "./ProcessDetail.module.css";

// State color mapping for process state characters.
function stateColor(state: string): string {
  switch (state) {
    case "R": return "var(--color-success)";
    case "D": case "Z": return "var(--color-danger)";
    case "T": return "var(--color-warning-text)";
    default: return "var(--color-text-muted)";
  }
}

interface Props {
  taskId: string;
  repo: string;
  branch: string;
  taskPath: string;
}

export default function ProcessDetail(props: Props) {
  const navigate = useNavigate();
  const [processes, setProcesses] = createSignal<ProcessInfo[] | null>(null);
  const [error, setError] = createSignal<string | null>(null);
  const [loading, setLoading] = createSignal(true);
  const [signallingPid, setSignallingPid] = createSignal<number | null>(null);

  const refresh = () => {
    setLoading(true);
    setError(null);
    getTaskProcesses(props.taskId)
      .then((resp) => setProcesses(resp.processes))
      .catch((e) => setError(e instanceof Error ? e.message : "Unknown error"))
      .finally(() => setLoading(false));
  };

  createEffect(refresh);

  const handleSignal = async (pid: number, sig: "SIGTERM" | "SIGKILL") => {
    setSignallingPid(pid);
    try {
      await signalProcess(props.taskId, String(pid), { signal: sig });
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to send signal");
    } finally {
      setSignallingPid(null);
    }
  };

  return (
    <div class={styles.container}>
      <div class={styles.header}>
        <button class={styles.backBtn} onClick={() => navigate(props.taskPath)} title="Back to task">
          <ArrowBackIcon width={20} height={20} />
        </button>
        <span class={styles.headerMeta}>
          <span class={styles.headerRepo}>{props.repo}</span>
          <span class={styles.headerBranch}>{props.branch}</span>
        </span>
      </div>
      <div class={styles.content}>
        <Show when={loading()}>
          <div class={styles.loading}>Loading processes...</div>
        </Show>
        <Show when={error()}>
          <div class={styles.error}>{error()}</div>
        </Show>
        <Show when={!loading() && !error()}>
          <Show when={processes()} keyed fallback={
            <div class={styles.empty}>No running processes</div>
          }>
            {(procs) => (
              <Show when={procs.length > 0} fallback={
                <div class={styles.empty}>No running processes</div>
              }>
                <table class={styles.table}>
                  <thead>
                    <tr>
                      <th class={`${styles.th} ${styles.actionsHdr}`}>ACTIONS</th>
                      <th class={styles.th}>PID</th>
                      <th class={styles.th}>S</th>
                      <th class={styles.th}>CPU</th>
                      <th class={styles.th}>MEM</th>
                      <th class={styles.th}>TIME</th>
                      <th class={styles.th}>COMMAND</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={procs}>
                      {(p) => (
                        <tr>
                          <td class={`${styles.td} ${styles.actions}`}>
                            <button
                              class={styles.signalBtn}
                              onClick={() => handleSignal(p.pid, "SIGTERM")}
                              disabled={signallingPid() === p.pid}
                              title="Send SIGTERM (graceful termination)"
                            >
                              TERM
                            </button>
                            <button
                              class={`${styles.signalBtn} ${styles.signalKill}`}
                              onClick={() => handleSignal(p.pid, "SIGKILL")}
                              disabled={signallingPid() === p.pid}
                              title="Send SIGKILL (force kill)"
                            >
                              KILL
                            </button>
                          </td>
                          <td class={styles.td}>{p.pid}</td>
                          <td class={styles.td}>
                            <span class={styles.state} style={{ color: stateColor(p.state) }}>{p.state}</span>
                          </td>
                          <td class={styles.td}>{p.cpu.toFixed(1)}</td>
                          <td class={styles.td}>{p.mem.toFixed(1)}</td>
                          <td class={styles.td}>{p.time}</td>
                          <td class={`${styles.td} ${styles.cmd}`}>{p.command}</td>
                        </tr>
                      )}
                    </For>
                  </tbody>
                </table>
              </Show>
            )}
          </Show>
        </Show>
      </div>
    </div>
  );
}
