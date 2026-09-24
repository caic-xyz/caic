// Caic browser voice integration: gateway setup, task updates, and numbering around the Go Mode overlay.

import { createEffect, createSignal, onCleanup, Show, untrack, type Accessor } from "solid-js";
import type { Task } from "@sdk/types.gen";

import { useAppState } from "./AppState";
import { TaskNumberMap } from "./TaskNumberMap";
import { useHostMode } from "@maruel/gomode/web/HostMode";
import VoiceOverlay from "@maruel/gomode/web/VoiceOverlay";
import { configureVoiceGateway, voiceSession } from "@maruel/gomode/web/VoiceSession";
import { notifications } from "@maruel/gomode/web/notifications";
import { setVoiceConnected, setVoiceTaskNumberMap } from "./voiceTaskState";
import { buildTaskCIContext, buildTaskStateContext } from "./voiceTaskContext";

type VoiceGatewayManifest = { service: string; webShell: { voiceGateway: { url?: string; tokenEndpoint?: string } } };

export function resolveVoiceGateway(
  manifest: VoiceGatewayManifest,
  origin: string,
): {
  url: string;
  tokenEndpoint: string | null;
} {
  if (manifest.service !== "caic") throw new Error("Go Mode manifest is for another service");
  const gateway = manifest.webShell.voiceGateway;
  if (!gateway.url) throw new Error("Go Mode manifest has no voice gateway URL");
  const gatewayURL = new URL(gateway.url, origin);
  if (gatewayURL.origin === origin) return { url: gateway.url, tokenEndpoint: null };

  const endpoint = gateway.tokenEndpoint;
  if (!endpoint?.startsWith("/") || endpoint.includes("\\")) {
    throw new Error("External voice gateway requires a same-origin token endpoint");
  }
  const endpointURL = new URL(endpoint, origin);
  if (endpointURL.origin !== origin) {
    throw new Error("External voice gateway requires a same-origin token endpoint");
  }
  return { url: gateway.url, tokenEndpoint: endpointURL.href };
}

/** Browser-owned shell features that Android Go Mode owns natively in host mode. */
export default function BrowserVoiceShell() {
  const s = useAppState();
  const hostMode = useHostMode();
  const [voiceReady, setVoiceReady] = createSignal(false);
  const [voiceError, setVoiceError] = createSignal("");

  createEffect(() => {
    if (!hostMode.browserVoiceEnabled() || !s.voiceGatewayAvailable()) return;
    let active = true;
    async function configure() {
      const response = await fetch("/.well-known/gomode.json", { cache: "no-store" });
      if (!response.ok) throw new Error(`Go Mode manifest returned HTTP ${response.status}`);
      const manifest = (await response.json()) as VoiceGatewayManifest;
      if (!active) return;
      const gateway = resolveVoiceGateway(manifest, window.location.origin);

      if (!gateway.tokenEndpoint) {
        configureVoiceGateway(gateway.url, null);
      } else {
        const tokenEndpoint = gateway.tokenEndpoint;
        configureVoiceGateway(gateway.url, async () => {
          const tokenResponse = await fetch(tokenEndpoint, { credentials: "same-origin", cache: "no-store" });
          if (!tokenResponse.ok) throw new Error(`Voice token request returned HTTP ${tokenResponse.status}`);
          return (await tokenResponse.json()) as {
            kind: string;
            instanceID: string;
            baseURL: string;
            token: string;
          };
        });
      }
      if (active) setVoiceReady(true);
    }
    void configure().catch((error: unknown) => {
      console.error("Could not configure caic voice gateway", error);
      if (active) setVoiceError("Voice gateway setup failed. Reload the page to retry.");
    });
    onCleanup(() => {
      active = false;
    });
  });

  // This shell is owned by one confirmed account. The Go Mode overlay keeps its
  // session globally, so explicitly end it before another account's shell mounts.
  onCleanup(() => {
    voiceSession.disconnect();
    notifications.setVoiceActive(false);
    setVoiceConnected(false);
    setVoiceTaskNumberMap(null);
  });

  return (
    <>
      <Show when={hostMode.browserVoiceEnabled() && s.voiceGatewayAvailable() && voiceReady()}>
        <VoiceTaskUpdates tasks={s.tasks} />
        <VoiceOverlay />
      </Show>
      <Show when={hostMode.browserVoiceEnabled() && s.voiceGatewayAvailable() && voiceError()}>
        <p role="alert">{voiceError()}</p>
      </Show>
    </>
  );
}

export function VoiceTaskUpdates(props: { tasks: Accessor<Task[]> }) {
  const numberMap = new TaskNumberMap();
  let prevStates = new Map<string, string>();
  let prevCIStatuses = new Map<string, string | undefined>();
  let wasConnected = false;

  // Seed current items at connection, without replaying them as new changes.
  createEffect(() => {
    const connected = voiceSession.state.connected;
    setVoiceConnected(connected);
    if (connected && !wasConnected) {
      const tasks = untrack(() => props.tasks());
      setVoiceTaskNumberMap(numberMap);
      prevStates = new Map(tasks.map((t) => [t.id, t.state]));
      prevCIStatuses = new Map(tasks.map((t) => [t.id, t.ciStatus]));
    }
    wasConnected = connected;
  });

  createEffect(() => {
    const currentTasks = props.tasks();
    numberMap.update(currentTasks);
    if (voiceSession.state.connected) {
      setVoiceTaskNumberMap(numberMap);
      const numbered = currentTasks.length > 1;
      for (const task of currentTasks) {
        const prev = prevStates.get(task.id);
        const taskNumber = numberMap.toNumber(task.id);
        if (prev !== undefined && prev !== task.state && taskNumber !== undefined) {
          const notification = buildTaskStateContext(task, taskNumber, numbered);
          if (notification !== null) voiceSession.injectText(notification);
        }
        const prevCI = prevCIStatuses.get(task.id);
        if (prevCI !== undefined && prevCI !== "failure" && task.ciStatus === "failure" && taskNumber !== undefined) {
          voiceSession.injectText(buildTaskCIContext(task, taskNumber, numbered));
        }
      }
    }
    prevStates = new Map(currentTasks.map((t) => [t.id, t.state]));
    prevCIStatuses = new Map(currentTasks.map((t) => [t.id, t.ciStatus]));
  });

  onCleanup(() => {
    setVoiceConnected(false);
    setVoiceTaskNumberMap(null);
  });
  return null;
}
