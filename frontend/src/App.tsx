// Application shell: top-level chrome (navbar, dialogs, voice, toasts) wrapping the routed panes.
import { createSignal, For, Show, type JSX } from "solid-js";
import { Portal } from "solid-js/web";
import type { Harness } from "@sdk/types.gen";
import { AppStateProvider, useAppState } from "./AppState";
import { effortOptions } from "./effortOptions";
import Dropdown from "./components/Dropdown";
import RepoChipStrip from "./components/RepoChipStrip";
import LoginPage from "./pages/LoginPage";
import AutoResizeTextarea from "./components/AutoResizeTextarea";
import Button from "./components/Button";
import UsageBadges from "./components/UsageBadges";
import VoiceOverlay from "./components/VoiceOverlay";
import CloneRepoDialog from "./components/CloneRepoDialog";
import USBIcon from "@material-symbols/svg-400/outlined/usb.svg?solid";
import DisplayIcon from "@material-symbols/svg-400/outlined/desktop_windows.svg?solid";
import SudoIcon from "@material-symbols/svg-400/outlined/shield_person.svg?solid";
import TokenIcon from "./components/github.svg?solid";
import PersonIcon from "@material-symbols/svg-400/outlined/person.svg?solid";
import SettingsIcon from "@material-symbols/svg-400/outlined/settings.svg?solid";
import TailscaleIcon from "./components/tailscale.svg?solid";
import styles from "./App.module.css";
import controls from "./controls.module.css";

function ConnectionDot(props: { connected: boolean }) {
  return (
    <span
      class={props.connected ? styles.dotConnected : styles.dotDisconnected}
      title={props.connected ? "Connected" : "Disconnected"}
      data-testid="connection-dot"
    />
  );
}

/** Top-level chrome: navbar, modals, overlays, and the routed detail panes. */
function Shell(props: { children?: JSX.Element }) {
  const s = useAppState();
  const auth = s.auth;

  return (
    <Show when={auth.providers().length === 0 || auth.user()} fallback={<LoginPage />}>
    <div class={styles.app}>
      <div class={styles.navbar}>
        <h1 class={styles.title}>
          <button class={styles.titleButton} type="button" onClick={() => s.navigate("/")}>caic</button>
        </h1>
        <span class={styles.subtitle}>Coding Agents in Containers</span>
        <UsageBadges usage={s.usage} now={s.now} />
        <ConnectionDot connected={s.connected()} />
        {(() => {
          const [menuOpen, setMenuOpen] = createSignal(false);
          const hasAuth = () => auth.providers().length > 0;
          const user = () => auth.user() ?? { username: "", avatarURL: undefined };
          const initials = () => user().username.slice(0, 2).toUpperCase();
          return (
            <Dropdown
              open={menuOpen()}
              onOpenChange={setMenuOpen}
              class={styles.userMenu}
              content={
                <div class={styles.userDropdown}>
                  <Show when={hasAuth() && auth.user()}>
                    <span class={styles.dropdownUser}>{user().username}</span>
                  </Show>
                  <button type="button" class={styles.dropdownItem} onClick={() => { setMenuOpen(false); s.navigate("/settings"); }}>
                    <SettingsIcon width="1em" height="1em" style={{ "vertical-align": "middle", "margin-right": "0.4em" }} />
                    Settings
                  </button>
                  <Show when={hasAuth() && auth.user()}>
                    <button class={styles.dropdownItem} onClick={() => { setMenuOpen(false); void auth.logout(); }}>Sign out</button>
                  </Show>
                </div>
              }
            >
              <button
                class={styles.avatarButton}
                onClick={() => setMenuOpen((v) => !v)}
                title={hasAuth() && auth.user() ? user().username : "Menu"}
              >
                <Show when={hasAuth() && auth.user()} fallback={
                  <PersonIcon width="1.3em" height="1.3em" />
                }>
                  <Show when={user().avatarURL} keyed fallback={
                    <span class={styles.avatarInitials}>{initials()}</span>
                  }>
                    {(url) => <img src={url} alt={user().username} class={styles.avatarImg} />}
                  </Show>
                </Show>
              </button>
            </Dropdown>
          );
        })()}
      </div>

      {props.children}

      <Show when={s.cloneOpen()}>
        <CloneRepoDialog
          loading={s.cloning()}
          error={s.cloneError()}
          onClone={s.submitClone}
          onClose={() => { s.setCloneOpen(false); s.setCloneError(""); }}
        />
      </Show>

      <Show when={s.forkTaskId()}>
        {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions, jsx-a11y/click-events-have-key-events -- native <dialog> handles Escape; click-to-dismiss on padding is supplementary */}
        <dialog
          ref={(el) => {
            el.addEventListener("close", () => { s.setForkTaskId(null); });
            const stopEscape = (e: KeyboardEvent) => {
              if (e.key === "Escape") { e.stopPropagation(); e.stopImmediatePropagation(); }
            };
            el.addEventListener("keydown", stopEscape, true);
            queueMicrotask(() => el.showModal());
          }}
          class={styles.forkDialog}
          onClick={(e) => { if (e.target === e.currentTarget) s.setForkTaskId(null); }}
        >
          <h2 class={styles.forkTitle}>Fork task</h2>
          <AutoResizeTextarea
            value={s.forkPrompt()}
            onInput={s.setForkPrompt}
            onSubmit={s.submitFork}
            placeholder="Prompt for forked task"
            class={styles.forkInput}
            tabIndex={0}
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
            <Show when={s.harnesses().length > 1}>
              <select
                value={s.forkHarness()}
                onChange={(e) => {
                  const h = e.currentTarget.value;
                  s.setForkHarness(h);
                  const models = s.harnesses().find((x) => x.name === h)?.models ?? [];
                  s.setForkModel(models.includes(s.forkModel()) ? s.forkModel() : "");
                  s.setForkEffort("");
                }}
                class={controls.modelSelect}
              >
                <For each={s.harnesses()}>
                  {(h) => <option value={h.name}>{h.name}</option>}
                </For>
              </select>
            </Show>
            <Show when={(s.harnesses().find((h) => h.name === s.forkHarness())?.models ?? []).length > 0}>
              <select
                value={s.forkModel()}
                onChange={(e) => s.setForkModel(e.currentTarget.value)}
                class={controls.modelSelect}
              >
                <option value="">Default model</option>
                <For each={s.harnesses().find((h) => h.name === s.forkHarness())?.models ?? []}>
                  {(m) => <option value={m}>{m}</option>}
                </For>
              </select>
            </Show>
            <Show when={effortOptions(s.forkHarness() as Harness).length > 0}>
              <select
                value={s.forkEffort()}
                onChange={(e) => s.setForkEffort(e.currentTarget.value)}
                class={controls.modelSelect}
              >
                <option value="">Default effort</option>
                <For each={effortOptions(s.forkHarness() as Harness)}>
                  {(e) => <option value={e}>{e}</option>}
                </For>
              </select>
            </Show>
          </div>
          <div class={styles.forkRow}>
            <Show when={s.tailscaleAvailable()}>
              <label class={controls.toggleChip} title="Enable Tailscale networking">
                <input type="checkbox" checked={s.forkTailscale()} onChange={(e) => s.setForkTailscale(e.currentTarget.checked)} />
                <TailscaleIcon width="1.2em" height="1.2em" />
              </label>
            </Show>
            <Show when={s.usbAvailable()}>
              <label class={controls.toggleChip} title="Enable USB passthrough">
                <input type="checkbox" checked={s.forkUSB()} onChange={(e) => s.setForkUSB(e.currentTarget.checked)} />
                <USBIcon width="1.2em" height="1.2em" />
              </label>
            </Show>
            <Show when={s.displayAvailable()}>
              <label class={controls.toggleChip} title="Enable virtual display">
                <input type="checkbox" checked={s.forkDisplay()} onChange={(e) => s.setForkDisplay(e.currentTarget.checked)} />
                <DisplayIcon width="1.2em" height="1.2em" />
              </label>
            </Show>
            <Show when={s.sudoAvailable()}>
              <label class={controls.toggleChip} title="Enable root access">
                <input type="checkbox" checked={s.forkSudo()} onChange={(e) => s.setForkSudo(e.currentTarget.checked)} />
                <SudoIcon width="1.2em" height="1.2em" />
              </label>
            </Show>
            <Show when={s.gitHubTokenAvailable()}>
              <label class={controls.toggleChip} title="Enable GitHub token">
                <input type="checkbox" checked={s.forkGitHubToken()} onChange={(e) => s.setForkGitHubToken(e.currentTarget.checked)} />
                <TokenIcon width="1.2em" height="1.2em" />
              </label>
            </Show>
          </div>
          <div class={styles.forkActions}>
            <button type="button" class={styles.forkCancel} onClick={() => s.setForkTaskId(null)}>Cancel</button>
            <Button type="button" onClick={s.submitFork} disabled={!s.forkPrompt().trim()}>Fork</Button>
          </div>
        </dialog>
      </Show>

      <VoiceOverlay
        tasks={s.tasks}
        recentRepo={() => s.repos()[0]?.path ?? ""}
        selectedHarness={s.selectedHarness}
        selectedModel={s.selectedModel}
        serverCaps={s.serverCaps}
      />
      <Portal>
        <div class={styles.toastContainer}>
          <For each={s.warnings()}>
            {(w) => (
              <div class={styles.toast}>
                <span>{w.message}</span>
                <button class={styles.toastDismiss} onClick={() => s.dismissWarning(w.id)}>×</button>
              </div>
            )}
          </For>
        </div>
      </Portal>
    </div>
    </Show>
  );
}

/** Router layout for "/": provides the app store and renders the shell around routed panes. */
export default function App(props: { children?: JSX.Element }) {
  return (
    <AppStateProvider>
      <Shell>{props.children}</Shell>
    </AppStateProvider>
  );
}
