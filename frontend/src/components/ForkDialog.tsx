// Fork-task modal dialog: prompt, extra repos, harness/model/effort, runtime toggles.

import { Show } from "solid-js";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import SudoIcon from "@material-symbols/svg-400/outlined/shield_person.svg?solid";

import { useAppState } from "../AppState";
import RepoChipStrip from "./RepoChipStrip";
import AutoResizeTextarea from "./AutoResizeTextarea";
import Button from "./Button";
import { HarnessControls, ToggleChip } from "./FormControls";
import ModalDialog from "./ModalDialog";
import TokenIcon from "./github.svg?solid";
import TailscaleIcon from "./tailscale.svg?solid";
import styles from "./ForkDialog.module.css";

export default function ForkDialog() {
  const s = useAppState();
  return (
    <Show when={s.forkTaskId()}>
      <ModalDialog
        class={styles.forkDialog}
        data-testid="fork-dialog"
        onClose={() => s.setForkTaskId(null)}
      >
        <h2 class={styles.forkTitle}>Fork task</h2>
        <AutoResizeTextarea
          value={s.forkPrompt()}
          onInput={s.setForkPrompt}
          onSubmit={s.submitFork}
          placeholder="Prompt for forked task"
          class={styles.forkInput}
          tabIndex={0}
          data-testid="fork-prompt-input"
        />
        <Show when={s.forkAvailableRecent().length > 0 || s.forkAvailableRest().length > 0 || s.forkExtraRepos().length > 0}>
          <RepoChipStrip
            repos={s.repos}
            selectedRepos={s.forkExtraRepos}
            onAdd={(path) => s.setForkExtraRepos((prev) => [...prev, { path, branch: "" }])}
            onRemove={(path) => s.setForkExtraRepos((prev) => prev.filter((r) => r.path !== path))}
            onSetBranch={(path, branch) => s.setForkExtraRepos((prev) => prev.map((r) => r.path === path ? { ...r, branch } : r))}
            availableRecent={s.forkAvailableRecent}
            availableRest={s.forkAvailableRest}
            showClone={false}
          />
        </Show>
        <div class={styles.forkRow}>
          <HarnessControls
            labelPrefix="Fork "
            harnesses={s.harnesses()}
            harness={s.forkHarness()}
            model={s.forkModel()}
            effort={s.forkEffort()}
            onHarness={s.setForkHarness}
            onModel={s.setForkModel}
            onEffort={s.setForkEffort}
          />
        </div>
        <div class={styles.forkRow}>
          <Show when={s.tailscaleAvailable()}>
            <ToggleChip checked={s.forkTailscale()} title="Enable Tailscale networking" onChange={s.setForkTailscale}>
              <TailscaleIcon width="1.2em" height="1.2em" />
            </ToggleChip>
          </Show>
          <Show when={s.usbAvailable()}>
            <ToggleChip checked={s.forkUSB()} title="Enable USB passthrough" onChange={s.setForkUSB}>
              <USBIcon width="1.2em" height="1.2em" />
            </ToggleChip>
          </Show>
          <Show when={s.displayAvailable()}>
            <ToggleChip checked={s.forkDisplay()} title="Enable virtual display" onChange={s.setForkDisplay}>
              <DisplayIcon width="1.2em" height="1.2em" />
            </ToggleChip>
          </Show>
          <Show when={s.sudoAvailable()}>
            <ToggleChip checked={s.forkSudo()} title="Enable root access" onChange={s.setForkSudo}>
              <SudoIcon width="1.2em" height="1.2em" />
            </ToggleChip>
          </Show>
          <Show when={s.gitHubTokenAvailable()}>
            <ToggleChip checked={s.forkGitHubToken()} title="Enable GitHub token" onChange={s.setForkGitHubToken}>
              <TokenIcon width="1.2em" height="1.2em" />
            </ToggleChip>
          </Show>
        </div>
        <div class={styles.forkActions}>
          <button type="button" class={styles.forkCancel} onClick={() => s.setForkTaskId(null)}>Cancel</button>
          <Button type="button" onClick={s.submitFork} disabled={!s.forkPrompt().trim()} data-testid="fork-submit">Fork</Button>
        </div>
      </ModalDialog>
    </Show>
  );
}
