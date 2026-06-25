// Application shell: top-level chrome (navbar, dialogs, voice, toasts) wrapping the routed panes.
import { ErrorBoundary, Show, type JSX } from "solid-js";
import { AppStateProvider, useAppState } from "./AppState";
import LoginPage from "./pages/LoginPage";
import AccountMenu from "./components/AccountMenu";
import Button from "./components/Button";
import ForkDialog from "./components/ForkDialog";
import Toasts from "./components/Toasts";
import UsageBadges from "./components/UsageBadges";
import VoiceOverlay from "./components/VoiceOverlay";
import CloneRepoDialog from "./components/CloneRepoDialog";
import { HostModeProvider, useHostMode } from "./hostMode";
import styles from "./App.module.css";

/** Fallback UI shown when an ErrorBoundary catches a render error. */
function ErrorFallback(props: { error: unknown; reset: () => void }) {
  const message = () => (props.error instanceof Error ? props.error.message : String(props.error));
  return (
    <div class={styles.errorFallback} role="alert" data-testid="error-fallback">
      <p class={styles.errorTitle}>Something went wrong.</p>
      <pre class={styles.errorMessage}>{message()}</pre>
      <Button variant="gray" onClick={props.reset}>Try again</Button>
    </div>
  );
}

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
  const hostMode = useHostMode();

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
        <AccountMenu />
      </div>

      <ErrorBoundary fallback={(error, reset) => <ErrorFallback error={error} reset={reset} />}>
        {props.children}
      </ErrorBoundary>

      <Show when={s.cloneOpen()}>
        <CloneRepoDialog
          loading={s.cloning()}
          error={s.cloneError()}
          onClone={s.submitClone}
          onClose={() => { s.setCloneOpen(false); s.setCloneError(""); }}
        />
      </Show>

      <ForkDialog />

      <Show when={hostMode.browserVoiceEnabled() && s.voiceGatewayAvailable()}>
        <VoiceOverlay
          tasks={s.tasks}
          recentRepo={() => s.repos()[0]?.path ?? ""}
          selectedHarness={s.selectedHarness}
          selectedModel={s.selectedModel}
        />
      </Show>
      <Toasts />
    </div>
    </Show>
  );
}

/** Router layout for "/": provides the app store and renders the shell around routed panes. */
export default function App(props: { children?: JSX.Element }) {
  return (
    <ErrorBoundary fallback={(error, reset) => <ErrorFallback error={error} reset={reset} />}>
      <HostModeProvider>
        <AppStateProvider>
          <Shell>{props.children}</Shell>
        </AppStateProvider>
      </HostModeProvider>
    </ErrorBoundary>
  );
}
