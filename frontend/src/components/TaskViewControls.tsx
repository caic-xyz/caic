// Shared task header actions for full screen and desktop closing.
import { Show, type Accessor } from "solid-js";
import { useSearchParams } from "@solidjs/router";
import CloseIcon from "@material-symbols/svg-400/outlined/close.svg?solid";
import FullscreenIcon from "@material-symbols/svg-400/outlined/fullscreen.svg?solid";
import FullscreenExitIcon from "@material-symbols/svg-400/outlined/fullscreen_exit.svg?solid";
import ActionButton from "./ActionButton";
import styles from "./TaskViewControls.module.css";

export function useTaskFullscreenToggle(fullscreen: Accessor<boolean>): () => void {
  const [, setQuery] = useSearchParams();
  return () => setQuery({ fullscreen: fullscreen() ? undefined : "1" });
}

export default function TaskViewControls(props: {
  fullscreen: boolean;
  toggleFullscreen?: () => void;
  fullscreenRef?: (element: HTMLButtonElement) => void;
  close: () => void;
  closeLabel: string;
  closeTitle?: string;
  closeRef?: (element: HTMLButtonElement) => void;
}) {
  return (
    <div class={styles.controls}>
      <Show when={props.toggleFullscreen}>
        <ActionButton
          ref={props.fullscreenRef}
          onClick={props.toggleFullscreen}
          label={props.fullscreen ? "Exit full screen" : "Full screen"}
        >
          <Show when={props.fullscreen} fallback={<FullscreenIcon width={20} height={20} aria-hidden="true" />}>
            <FullscreenExitIcon width={20} height={20} aria-hidden="true" />
          </Show>
        </ActionButton>
      </Show>
      <ActionButton
        ref={props.closeRef}
        class={styles.closeButton}
        onClick={props.close}
        label={props.closeLabel}
        title={props.closeTitle}
      >
        <CloseIcon width={20} height={20} aria-hidden="true" />
      </ActionButton>
    </div>
  );
}
