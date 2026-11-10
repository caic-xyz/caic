// Adapts shared browser voice to caic authorization and UI task numbering.

import { createEffect, onCleanup, Show, type Accessor } from "solid-js";
import type { Task } from "@sdk/types.gen";
import SharedBrowserVoiceShell from "@maruel/gomode/web/BrowserVoiceShell";
import type { ServiceAuthorization } from "@maruel/gomode/sdk/voicegateway/v2/types.gen.ts";
import type { voiceSession } from "@maruel/gomode/web/VoiceSession";
import { useAppState } from "./AppState";
import { TaskNumberMap } from "./TaskNumberMap";
import { setVoiceConnected, setVoiceTaskNumberMap } from "./voiceTaskState";

export default function BrowserVoiceShell() {
  const s = useAppState();
  return (
    <Show when={s.voiceGatewayAvailable()}>
      <Show when={s.frontendPreparation()} keyed>
        {(preparation) => (
          <SharedBrowserVoiceShell
            service="caic"
            toolGroup="tasks"
            clientName="caic-frontend"
            preparation={preparation}
            authorize={authorizeVoice}
            setupFailed="Voice assistant unavailable."
            onError={(error) => console.error("Could not configure caic voice gateway", error)}
          >
            {({ session }) => <VoiceTaskUpdates tasks={s.tasks} session={session} />}
          </SharedBrowserVoiceShell>
        )}
      </Show>
    </Show>
  );
}

export function VoiceTaskUpdates(props: { tasks: Accessor<Task[]>; session: typeof voiceSession }) {
  // The MCP server owns model context. This adapter only maintains screen-facing numbers.
  const numberMap = new TaskNumberMap();
  createEffect(() => {
    const connected = props.session.state.connected;
    numberMap.update(props.tasks());
    setVoiceConnected(connected);
    setVoiceTaskNumberMap(connected ? numberMap : null);
  });

  onCleanup(() => {
    setVoiceConnected(false);
    setVoiceTaskNumberMap(null);
  });
  return null;
}

async function authorizeVoice(endpoint: string): Promise<ServiceAuthorization> {
  const response = await fetch(endpoint, { credentials: "same-origin", cache: "no-store" });
  if (!response.ok) throw new Error(`Voice token request returned HTTP ${response.status}`);
  return (await response.json()) as ServiceAuthorization;
}
