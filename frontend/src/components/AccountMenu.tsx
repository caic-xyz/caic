// Navbar account menu: avatar button with a dropdown for settings and sign-out.
import { createSignal, Show } from "solid-js";
import { useAppState } from "../AppState";
import Dropdown from "./Dropdown";
import PersonIcon from "@material-symbols/svg-400/outlined/person.svg?solid";
import SettingsIcon from "@material-symbols/svg-400/outlined/settings.svg?solid";
import styles from "./AccountMenu.module.css";

export default function AccountMenu() {
  const s = useAppState();
  const auth = s.auth;
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
}
