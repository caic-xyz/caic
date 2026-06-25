// Browser-side Go Mode shell capabilities for the hosted caic frontend.

import { Show } from "solid-js";

import { useAppState } from "../AppState";

import VoiceOverlay from "./VoiceOverlay";
import { useHostMode } from "./HostMode";

/** Browser-owned shell features that Android Go Mode owns natively in host mode. */
export default function GoModeBrowserShell() {
  const s = useAppState();
  const hostMode = useHostMode();

  return (
    <Show when={hostMode.browserVoiceEnabled() && s.voiceGatewayAvailable()}>
      <VoiceOverlay
        tasks={s.tasks}
        recentRepo={() => s.repos()[0]?.path ?? ""}
        selectedHarness={s.selectedHarness}
        selectedModel={s.selectedModel}
      />
    </Show>
  );
}
