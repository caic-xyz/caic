// Shared form control components for task harness selects and capability toggles.
import { splitProps, type JSX } from "solid-js";
import styles from "./FormControls.module.css";

type ControlSelectProps = JSX.SelectHTMLAttributes<HTMLSelectElement>;

function classes(base: string, extra: string | undefined) {
  return extra ? `${base} ${extra}` : base;
}

export function ControlSelect(props: ControlSelectProps) {
  const [local, rest] = splitProps(props, ["class", "children"]);

  return (
    <select {...rest} class={classes(styles.select, local.class)}>
      {local.children}
    </select>
  );
}

export function ToggleChip(props: {
  checked: boolean;
  children: JSX.Element;
  disabled?: boolean;
  title: string;
  onChange: (checked: boolean) => void;
}) {
  return (
    <label class={styles.toggleChip} title={props.title}>
      <input
        type="checkbox"
        checked={props.checked}
        disabled={props.disabled}
        aria-label={props.title}
        onChange={(e) => props.onChange(e.currentTarget.checked)}
      />
      {props.children}
    </label>
  );
}
