// Default layout: the new-task form, the sidebar task list, and the routed detail pane.

import { type JSX } from "solid-js";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import SudoIcon from "@material-symbols/svg-400/outlined/shield_person.svg?solid";

import { voiceConnected, getVoiceTaskNumber } from "../gomode/VoiceState";

import RepoChipStrip from "./RepoChipStrip";
import PromptInput from "./PromptInput";
import Button from "./Button";
import TaskList from "./TaskList";
import { useAppState } from "../AppState";
import { HarnessControls, ToggleChip } from "./FormControls";
import { Layout } from "./Layout";
import TokenIcon from "./github.svg?solid";
import TailscaleIcon from "./tailscale.svg?solid";
import styles from "./MainLayout.module.css";

export default function MainLayout(props: { children?: JSX.Element }) {
  const s = useAppState();

  return (
    <>
      <form onSubmit={(e) => { e.preventDefault(); s.submitTask(); }} class={`${styles.submitForm} ${s.selectedId() ? styles.hidden : ""}`}>
        <RepoChipStrip
          repos={s.repos}
          selectedRepos={s.selectedRepos}
          onAdd={(path) => { if (!s.selectedRepos().some((r) => r.path === path)) s.setSelectedRepos((prev) => [...prev, { path, branch: "" }]); }}
          onRemove={(path) => s.setSelectedRepos((prev) => prev.filter((r) => r.path !== path))}
          onSetBranch={(path, branch) => s.setSelectedRepos((prev) => prev.map((r) => r.path === path ? { ...r, branch } : r))}
          availableRecent={s.availableRecent}
          availableRest={s.availableRest}
          onClone={() => { s.setCloneOpen(true); s.setCloneError(""); }}
          data-testid="repo-chips"
        />
        <HarnessControls
          harnesses={s.harnesses()}
          harness={s.selectedHarness()}
          model={s.selectedModel()}
          effort={s.selectedEffort()}
          onHarness={s.setSelectedHarness}
          onModel={s.setSelectedModel}
          onEffort={s.setSelectedEffort}
        />
        <ToggleChip
          checked={s.tailscaleEnabled()}
          disabled={!s.tailscaleAvailable()}
          title={s.tailscaleAvailable() ? "Enable Tailscale networking" : "Tailscale is not available on this server"}
          onChange={s.setTailscaleEnabled}
        >
          <TailscaleIcon width="1.2em" height="1.2em" />
        </ToggleChip>
        <ToggleChip
          checked={s.usbEnabled()}
          disabled={!s.usbAvailable()}
          title={s.usbAvailable() ? "Enable USB passthrough" : "USB passthrough is not available on this server"}
          onChange={s.setUSBEnabled}
        >
          <USBIcon width="1.2em" height="1.2em" />
        </ToggleChip>
        <ToggleChip
          checked={s.displayEnabled()}
          disabled={!s.displayAvailable()}
          title={s.displayAvailable() ? "Enable virtual display" : "Virtual display is not available on this server"}
          onChange={s.setDisplayEnabled}
        >
          <DisplayIcon width="1.2em" height="1.2em" />
        </ToggleChip>
        <ToggleChip
          checked={s.sudoEnabled()}
          disabled={!s.sudoAvailable()}
          title={s.sudoAvailable() ? "Enable root access" : "Root access (sudo) is not available on this server"}
          onChange={s.setSudoEnabled}
        >
          <SudoIcon width="1.2em" height="1.2em" />
        </ToggleChip>
        <ToggleChip
          checked={s.gitHubTokenEnabled()}
          disabled={!s.gitHubTokenAvailable()}
          title={s.gitHubTokenAvailable() ? "Enable GitHub token" : "GitHub token is not available on this server"}
          onChange={s.setGitHubTokenEnabled}
        >
          <TokenIcon width="1.2em" height="1.2em" />
        </ToggleChip>
        <PromptInput
          value={s.prompt()}
          onInput={s.setPrompt}
          onSubmit={s.submitTask}
          placeholder="Describe a task..."
          class={styles.promptInput}
          data-testid="prompt-input"
          supportsImages={s.harnessSupportsImages()}
          images={s.pendingImages()}
          onImagesChange={s.setPendingImages}
          sendButton={
            <Button type="submit" disabled={s.initializing() || s.submitting() || (!s.prompt().trim() && s.pendingImages().length === 0)} loading={s.initializing() || s.submitting()} title="Start a new container with this prompt" data-testid="submit-task">
              <SendIcon width="1.2em" height="1.2em" />
            </Button>
          }
        />
      </form>

      <Layout>
        <TaskList
          tasks={s.tasks}
          repos={s.repos}
          usage={s.usage}
          selectedId={s.selectedId()}
          sidebarOpen={s.sidebarOpen}
          setSidebarOpen={s.setSidebarOpen}
          now={s.now}
          onSelect={s.navigateToTask}
          onStop={s.handleStop}
          onPurge={s.handlePurge}
          onRevive={s.handleRevive}
          actionId={s.actionId}
          onDiffClick={s.navigateToDiff}
          autoFixCI={s.autoFixCI}
          autoFixPR={s.autoFixPR}
          onFixCI={s.fixCI}
          voiceConnected={voiceConnected}
          getTaskNumber={getVoiceTaskNumber}
        />
        {props.children}
      </Layout>
    </>
  );
}
