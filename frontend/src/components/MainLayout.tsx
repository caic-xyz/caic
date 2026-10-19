// Default layout: task creation with collapsible features and sidebar/detail panes with voice task numbers.

import { type JSX, For, Show, createSignal, createEffect, onCleanup } from "solid-js";
import { useLocation } from "@solidjs/router";
import { taskViewMode } from "../taskView";
import ChevronRightIcon from "@material-symbols/svg-400/outlined/chevron_right.svg?solid";
import SendIcon from "@material-symbols/svg-400/outlined/send.svg?solid";
import LoadingIcon from "@material-symbols/svg-400/outlined/progress_activity.svg?solid";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import SudoIcon from "@material-symbols/svg-400/outlined/shield_person.svg?solid";
import AccountTreeIcon from "@material-symbols/svg-400/outlined/account_tree.svg?solid";

import { voiceConnected, getVoiceTaskNumber } from "../voiceTaskState";

import RepoChipStrip from "./RepoChipStrip";
import PromptInput from "./PromptInput";
import ActionButton from "./ActionButton";
import TaskList from "./TaskList";
import { useAppState } from "../AppState";
import { ControlSelect, HarnessControls, ToggleChip } from "./FormControls";
import { Layout } from "./Layout";
import TokenIcon from "./github.svg?solid";
import TailscaleIcon from "./tailscale.svg?solid";
import { bindPromptSubmitShortcut } from "./promptSubmitShortcut";
import styles from "./MainLayout.module.css";

export default function MainLayout(props: { children?: JSX.Element }) {
  const s = useAppState();
  const location = useLocation();
  const mode = () => taskViewMode(location.pathname, location.search);
  const fullscreen = () => mode() === "fullscreen";
  const media = window.matchMedia("(max-width: 1279px)");
  const [compact, setCompact] = createSignal(media.matches);
  const [sidebarOverride, setSidebarOverride] = createSignal<boolean | null>(null);
  const updateWidth = () => {
    setCompact(media.matches);
    setSidebarOverride(null);
  };
  media.addEventListener("change", updateWidth);
  onCleanup(() => media.removeEventListener("change", updateWidth));
  createEffect(() => {
    if (s.selectedId() === null) setSidebarOverride(null);
  });
  const sidebarOpen = () => s.selectedId() === null || (sidebarOverride() ?? (mode() === "closed" || !compact()));
  const modelSettingVisible = () =>
    s.harnesses().length > 1 ||
    (s.harnesses().find((harness) => harness.name === s.selectedHarness())?.models.length ?? 0) > 0;

  return (
    <>
      <form
        ref={(form) =>
          bindPromptSubmitShortcut(form, () => {
            const submit = form.querySelector<HTMLButtonElement>('button[type="submit"]');
            if (submit && !submit.disabled) form.requestSubmit(submit);
          })
        }
        onSubmit={(e) => {
          e.preventDefault();
          s.submitTask();
        }}
        class={`${styles.submitForm} ${s.selectedId() ? styles.hidden : ""}`}
        data-testid="new-task-form"
      >
        <RepoChipStrip
          repos={s.repos}
          selectedRepos={s.selectedRepos}
          onAdd={(path) => {
            if (!s.selectedRepos().some((r) => r.path === path))
              s.setSelectedRepos((prev) => [...prev, { path, branch: "" }]);
          }}
          onRemove={(path) => s.setSelectedRepos((prev) => prev.filter((r) => r.path !== path))}
          onSetBranch={(path, branch) =>
            s.setSelectedRepos((prev) => prev.map((r) => (r.path === path ? { ...r, branch } : r)))
          }
          availableRecent={s.availableRecent}
          availableRest={s.availableRest}
          onClone={() => {
            s.setCloneOpen(true);
            s.setCloneError("");
          }}
          data-testid="repo-chips"
        />
        <HarnessControls
          harnesses={s.harnesses()}
          harness={s.selectedHarness()}
          harnessKeyShortcuts="F3"
          model={s.selectedModel()}
          effort={s.selectedEffort()}
          onHarness={s.setSelectedHarness}
          onHarnessCommit={() =>
            requestAnimationFrame(() => document.querySelector<HTMLElement>("[data-testid='prompt-input']")?.focus())
          }
          onModel={s.setSelectedModel}
          onEffort={s.setSelectedEffort}
        />
        <button
          type="button"
          class={styles.featureToggle}
          aria-label={s.featuresOpen() ? "Hide features" : "Show features"}
          aria-expanded={s.featuresOpen()}
          aria-controls="new-task-features"
          title={s.featuresOpen() ? "Hide features" : "Show features"}
          onClick={() => s.setFeaturesOpen((open) => !open)}
        >
          <ChevronRightIcon
            width="1.2em"
            height="1.2em"
            aria-hidden="true"
            class={`${s.featuresOpen() ? styles.featureArrowOpen : ""}`}
          />
        </button>
        <div id="new-task-features" class={styles.featurePanel} hidden={!s.featuresOpen()}>
          {s.runtimes().length > 1 && (
            <ControlSelect
              aria-label="Runtime"
              aria-keyshortcuts={modelSettingVisible() ? undefined : "F3"}
              data-testid="runtime-select"
              title={modelSettingVisible() ? undefined : "Choose runtime (F3)"}
              value={s.selectedRuntimeName()}
              onChange={(e) => s.setSelectedRuntimeName(e.currentTarget.value)}
            >
              <For each={s.runtimes()}>
                {(rt) => (
                  <option value={rt.name} selected={rt.name === s.selectedRuntimeName()}>
                    {rt.name}
                  </option>
                )}
              </For>
            </ControlSelect>
          )}
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
          <ToggleChip checked={s.caicMCP()} title="Enable CAIC MCP delegation for this task" onChange={s.setCaicMCP}>
            <AccountTreeIcon width="1.2em" height="1.2em" />
          </ToggleChip>
          <input
            class={styles.sessionInput}
            type="text"
            aria-label="Session ID to resume"
            placeholder="Session ID to resume"
            value={s.resumeSessionID()}
            onInput={(e) => s.setResumeSessionID(e.currentTarget.value)}
            autocomplete="off"
            spellcheck={false}
          />
        </div>
        <PromptInput
          value={s.prompt()}
          onInput={s.setPrompt}
          onSubmit={s.submitTask}
          placeholder="Describe a task..."
          class={styles.promptInput}
          data-testid="prompt-input"
          supportsImages={s.harnessSupportsImages()}
          images={s.pendingImages()}
          imageConstraints={s.imageConstraints()}
          imageGeneration={s.pendingImageGeneration()}
          onAddImages={s.addPendingImages}
          onRemoveImage={(image) => s.removePendingImages([image])}
          sendButton={
            <ActionButton
              label={s.resumeSessionID().trim() ? "Resume session" : "Create task"}
              type="submit"
              disabled={s.initializing() || s.submitting() || (!s.prompt().trim() && s.pendingImages().length === 0)}
              aria-busy={s.initializing() || s.submitting()}
              title={
                s.resumeSessionID().trim()
                  ? "Resume session in a new container with this prompt"
                  : "Start a new container with this prompt"
              }
              data-testid="submit-task"
            >
              <Show
                when={!s.initializing() && !s.submitting()}
                fallback={<LoadingIcon class={styles.submitLoadingIcon} width="20" height="20" />}
              >
                <SendIcon class={styles.submitIcon} width="20" height="20" />
              </Show>
            </ActionButton>
          }
        />
      </form>

      <Layout>
        <div
          class={`${styles.summaryPane} ${s.selectedId() !== null ? styles.summarySelected : ""} ${fullscreen() ? styles.summaryHidden : ""}`}

          inert={fullscreen()}
          aria-hidden={fullscreen()}
          data-testid="summary-pane"
        >
          <TaskList
            tasks={s.tasks}
            tasksLoading={s.tasksLoading}
            settledLoading={s.settledLoading}
            repos={s.repos}
            usage={s.usage}
            selectedId={s.selectedId()}
            sidebarOpen={sidebarOpen}
            setSidebarOpen={setSidebarOverride}
            now={s.now}
            onSelect={s.navigateToTask}
            onStop={s.handleStop}
            onPurge={s.handlePurge}
            onRevive={s.handleRevive}
            onFork={s.handleFork}
            onQuotaRecovery={s.handleQuotaRecovery}
            isQuotaDismissed={s.isQuotaWarningDismissed}
            onDismissQuotaWarning={s.dismissQuotaWarning}
            onError={s.showWarning}
            supportsCompact={(harness) =>
              s.harnesses().find((candidate) => candidate.name === harness)?.supportsCompact ?? false
            }
            harnessLogoUrl={(harness) => s.harnesses().find((candidate) => candidate.name === harness)?.logoUrl}
            actionId={s.actionId}
            autoFixCI={s.autoFixCI}
            autoFixPR={s.autoFixPR}
            onFixCI={s.fixCI}
            voiceConnected={voiceConnected}
            getTaskNumber={getVoiceTaskNumber}
          />
        </div>
        {props.children}
      </Layout>
    </>
  );
}
