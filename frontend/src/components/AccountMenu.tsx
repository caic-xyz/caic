// Navbar account menu: avatar button with a dropdown for settings and sign-out.

import { createSignal, Show } from "solid-js";
import { A } from "@solidjs/router";
import PersonIcon from "@material-symbols/svg-400/outlined/person.svg?solid";
import SettingsIcon from "@material-symbols/svg-400/outlined/settings.svg?solid";

import { useAppState } from "../AppState";
import Dropdown from "./Dropdown";
import styles from "./AccountMenu.module.css";

export default function AccountMenu(props: { onKeyboardShortcuts: () => void }) {
  const auth = useAppState().auth;
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
        <div class={styles.userDropdown} data-testid="account-menu">
          <Show when={hasAuth() && auth.user()}>
            <span class={styles.dropdownUser}>{user().username}</span>
          </Show>
          <A class={styles.dropdownItem} href="/settings" onClick={() => setMenuOpen(false)}>
            <SettingsIcon width="1em" height="1em" style={{ "vertical-align": "middle", "margin-right": "0.4em" }} />
            Settings
          </A>
          <button
            type="button"
            class={styles.dropdownItem}
            title="Keyboard shortcuts (? or F1)"
            aria-keyshortcuts="? F1"
            onClick={() => { setMenuOpen(false); props.onKeyboardShortcuts(); }}
          >
            Keyboard shortcuts
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
}
