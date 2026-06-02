// Default layout: the new-task form, the sidebar task list, and the routed detail pane.
import { For, Show, type JSX } from "solid-js";
import type { Harness } from "@sdk/types.gen";
import RepoChipStrip from "./RepoChipStrip";
import PromptInput from "./PromptInput";
import Button from "./Button";
import TaskList from "./TaskList";
import { useAppState } from "../AppState";
import { effortOptions } from "../effortOptions";
import { voiceConnected, getVoiceTaskNumber } from "../VoiceState";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import SudoIcon from "@material-symbols/svg-400/outlined/shield_person.svg?solid";
import TokenIcon from "./github.svg?solid";
import TailscaleIcon from "./tailscale.svg?solid";
import styles from "./MainLayout.module.css";
import controls from "../controls.module.css";
import layout from "../layout.module.css";

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
        <Show when={s.harnesses().length > 1}>
          <select
            value={s.selectedHarness()}
            onChange={(e) => {
              const h = e.currentTarget.value;
              s.setSelectedHarness(h);
              const models = s.harnesses().find((x) => x.name === h)?.models ?? [];
              const lastModel = s.getPrefModel(h);
              s.setSelectedModel(lastModel && models.includes(lastModel) ? lastModel : "");
              s.setSelectedEffort("");
            }}
            class={controls.modelSelect}
          >
            <For each={s.harnesses()}>
              {(h) => <option value={h.name}>{h.name}</option>}
            </For>
          </select>
        </Show>
        <Show when={(s.harnesses().find((h) => h.name === s.selectedHarness())?.models ?? []).length > 0}>
          <select
            value={s.selectedModel()}
            onChange={(e) => {
              const m = e.currentTarget.value;
              s.setSelectedModel(m);
              s.setPrefModel(s.selectedHarness(), m);
            }}
            class={controls.modelSelect}
          >
            <option value="">Default model</option>
            <For each={s.harnesses().find((h) => h.name === s.selectedHarness())?.models ?? []}>
              {(m) => <option value={m}>{m}</option>}
            </For>
          </select>
        </Show>
        <Show when={effortOptions(s.selectedHarness() as Harness).length > 0}>
          <select
            value={s.selectedEffort()}
            onChange={(e) => s.setSelectedEffort(e.currentTarget.value)}
            class={controls.modelSelect}
          >
            <option value="">Default effort</option>
            <For each={effortOptions(s.selectedHarness() as Harness)}>
              {(e) => <option value={e}>{e}</option>}
            </For>
          </select>
        </Show>
        <label class={controls.toggleChip} title={s.tailscaleAvailable() ? "Enable Tailscale networking" : "Tailscale is not available on this server"}>
          <input
            type="checkbox"
            checked={s.tailscaleEnabled()}
            disabled={!s.tailscaleAvailable()}
            onChange={(e) => s.setTailscaleEnabled(e.currentTarget.checked)}
          />
          <TailscaleIcon width="1.2em" height="1.2em" />
        </label>
        <label class={controls.toggleChip} title={s.usbAvailable() ? "Enable USB passthrough" : "USB passthrough is not available on this server"}>
          <input
            type="checkbox"
            checked={s.usbEnabled()}
            disabled={!s.usbAvailable()}
            onChange={(e) => s.setUSBEnabled(e.currentTarget.checked)}
          />
          <USBIcon width="1.2em" height="1.2em" />
        </label>
        <label class={controls.toggleChip} title={s.displayAvailable() ? "Enable virtual display" : "Virtual display is not available on this server"}>
          <input
            type="checkbox"
            checked={s.displayEnabled()}
            disabled={!s.displayAvailable()}
            onChange={(e) => s.setDisplayEnabled(e.currentTarget.checked)}
          />
          <DisplayIcon width="1.2em" height="1.2em" />
        </label>
        <label class={controls.toggleChip} title={s.sudoAvailable() ? "Enable root access" : "Root access (sudo) is not available on this server"}>
          <input
            type="checkbox"
            checked={s.sudoEnabled()}
            disabled={!s.sudoAvailable()}
            onChange={(e) => s.setSudoEnabled(e.currentTarget.checked)}
          />
          <SudoIcon width="1.2em" height="1.2em" />
        </label>
        <label class={controls.toggleChip} title={s.gitHubTokenAvailable() ? "Enable GitHub token" : "GitHub token is not available on this server"}>
          <input
            type="checkbox"
            checked={s.gitHubTokenEnabled()}
            disabled={!s.gitHubTokenAvailable()}
            onChange={(e) => s.setGitHubTokenEnabled(e.currentTarget.checked)}
          />
          <TokenIcon width="1.2em" height="1.2em" />
        </label>
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

      <div class={layout.layout}>
        <TaskList
          tasks={s.tasks}
          repos={s.repos}
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
      </div>
    </>
  );
}
