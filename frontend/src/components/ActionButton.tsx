// Clean toolbar action button with an accessible name, tooltip, and consistent interaction states.
import { splitProps, type JSX } from "solid-js";
import styles from "./ActionButton.module.css";

type Props = Omit<JSX.ButtonHTMLAttributes<HTMLButtonElement>, "aria-label"> & { label: string };

export default function ActionButton(props: Props) {
  const [local, rest] = splitProps(props, ["label", "children", "class", "title", "type"]);
  return (
    <button
      {...rest}
      type={local.type ?? "button"}
      class={`${styles.button}${local.class ? ` ${local.class}` : ""}`}
      aria-label={local.label}
      title={local.title ?? local.label}
    >
      <span class={styles.content} aria-hidden="true">
        {local.children}
      </span>
    </button>
  );
}
