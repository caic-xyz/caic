// Clean toolbar action button with an accessible name, tooltip, and consistent interaction states.
import { splitProps, Show, type JSX } from "solid-js";
import styles from "./ActionButton.module.css";

type Props = Omit<JSX.ButtonHTMLAttributes<HTMLButtonElement>, "aria-label"> & {
  label: string;
  // loading replaces the icon with a spinner and blocks a second request.
  loading?: boolean;
};

export default function ActionButton(props: Props) {
  const [local, rest] = splitProps(props, ["label", "children", "class", "title", "type", "loading", "disabled"]);
  return (
    <button
      {...rest}
      type={local.type ?? "button"}
      class={`${styles.button}${local.class ? ` ${local.class}` : ""}`}
      disabled={local.disabled || local.loading}
      aria-busy={local.loading ? true : undefined}
      aria-label={local.label}
      title={local.title ?? local.label}
    >
      <span class={styles.content} aria-hidden="true">
        <Show when={local.loading} fallback={local.children}>
          <span class={styles.spinner} />
        </Show>
      </span>
    </button>
  );
}
