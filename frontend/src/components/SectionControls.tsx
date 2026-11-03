// SectionControls renders a Monitoring section's controls into the tab bar's trailing slot.

import { createContext, Show, useContext, type JSX } from "solid-js";
import { Portal } from "solid-js/web";

export const ControlsHost = createContext<() => HTMLDivElement | undefined>();

export default function SectionControls(props: { children: JSX.Element }) {
  const host = useContext(ControlsHost);
  return (
    <Show when={host?.()} fallback={props.children}>
      {(mount) => <Portal mount={mount()}>{props.children}</Portal>}
    </Show>
  );
}
